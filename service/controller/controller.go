package controller

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/task"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/inbound"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/features/stats"

	"github.com/XrayR-project/XrayR/api"
	"github.com/XrayR-project/XrayR/app/mydispatcher"
	"github.com/XrayR-project/XrayR/common/mylego"
	"github.com/XrayR-project/XrayR/common/serverstatus"
)

type LimitInfo struct {
	end               int64
	currentSpeedLimit int
	originSpeedLimit  uint64
}

type Controller struct {
	server        *core.Instance
	config        *Config
	clientInfo    api.ClientInfo
	apiClient     api.API
	nodeInfo      *api.NodeInfo
	Tag           string
	userList      *[]api.UserInfo
	tasks         []periodicTask
	limitedUsers  map[api.UserInfo]LimitInfo
	warnedUsers   map[api.UserInfo]int
	panelType     string
	ibm           inbound.Manager
	obm           outbound.Manager
	stm           stats.Manager
	dispatcher    *mydispatcher.DefaultDispatcher
	startAt       time.Time
	lastOnlineLog time.Time // throttle: last time the online-user count was logged
	logger        *log.Entry
	monitorAccess sync.Mutex
	taskState     sync.Mutex
	taskWG        sync.WaitGroup
	closing       atomic.Bool
}

type periodicTask struct {
	tag string
	*task.Periodic
}

// New return a Controller service with default parameters.
func New(server *core.Instance, api api.API, config *Config, panelType string) *Controller {
	logger := log.NewEntry(log.StandardLogger()).WithFields(log.Fields{
		"Host": api.Describe().APIHost,
		"Type": api.Describe().NodeType,
		"ID":   api.Describe().NodeID,
	})
	controller := &Controller{
		server:    server,
		config:    config,
		apiClient: api,
		panelType: panelType,
		startAt:   time.Now(),
		logger:    logger,
	}
	controller.ibm, _ = server.GetFeature(inbound.ManagerType()).(inbound.Manager)
	controller.obm, _ = server.GetFeature(outbound.ManagerType()).(outbound.Manager)
	controller.stm, _ = server.GetFeature(stats.ManagerType()).(stats.Manager)
	controller.dispatcher, _ = server.GetFeature(routing.DispatcherType()).(*mydispatcher.DefaultDispatcher)

	return controller
}

// Start implement the Start() function of the service interface
func (c *Controller) Start() error {
	if c.ibm == nil || c.obm == nil || c.stm == nil || c.dispatcher == nil {
		return errors.New("core is missing the XrayR inbound, outbound, stats, or dispatcher feature")
	}
	c.closing.Store(false)
	c.clientInfo = c.apiClient.Describe()
	// First fetch Node Info
	newNodeInfo, err := c.apiClient.GetNodeInfo()
	if err != nil {
		return err
	}
	if newNodeInfo.Port == 0 {
		return errors.New("server port must > 0")
	}
	c.nodeInfo = newNodeInfo
	c.Tag = c.buildNodeTag()

	// Add new tag
	err = c.addNewTag(newNodeInfo)
	if err != nil {
		c.logger.Panic(err)
		return err
	}
	// Update user
	userInfo, err := c.apiClient.GetUserList()
	if err != nil {
		return err
	}

	// sync controller userList
	c.userList = userInfo

	err = c.addNewUser(userInfo, newNodeInfo)
	if err != nil {
		return err
	}

	// Add Limiter
	if err := c.AddInboundLimiter(c.Tag, newNodeInfo.SpeedLimit, userInfo, c.config.GlobalDeviceLimitConfig); err != nil {
		c.logger.Print(err)
	}

	// Add Rule Manager
	if !c.config.DisableGetRule {
		if ruleList, err := c.apiClient.GetNodeRule(); err != nil {
			c.logger.Printf("Get rule list filed: %s", err)
		} else {
			if ruleList == nil {
				return errors.New("panel returned a nil rule list")
			}
			if err := c.UpdateRule(c.Tag, *ruleList); err != nil {
				c.logger.Print(err)
			}
		}
	}

	// Init AutoSpeedLimitConfig
	if c.config.AutoSpeedLimitConfig == nil {
		c.config.AutoSpeedLimitConfig = &AutoSpeedLimitConfig{0, 0, 0, 0}
	}
	if c.config.AutoSpeedLimitConfig.Limit > 0 {
		c.limitedUsers = make(map[api.UserInfo]LimitInfo)
		c.warnedUsers = make(map[api.UserInfo]int)
	}

	// Add periodic tasks
	c.tasks = append(c.tasks,
		periodicTask{
			tag: "node monitor",
			Periodic: &task.Periodic{
				Interval: time.Duration(c.config.UpdatePeriodic) * time.Second,
				Execute:  func() error { return c.runPeriodic(c.nodeInfoMonitor) },
			}},
		periodicTask{
			tag: "user monitor",
			Periodic: &task.Periodic{
				Interval: time.Duration(c.config.UpdatePeriodic) * time.Second,
				Execute:  func() error { return c.runPeriodic(c.userInfoMonitor) },
			}},
	)

	// Check cert service in need
	if c.nodeInfo.EnableTLS && !c.config.EnableREALITY && !c.nodeInfo.EnableREALITY {
		c.tasks = append(c.tasks, periodicTask{
			tag: "cert monitor",
			Periodic: &task.Periodic{
				Interval: time.Duration(c.config.UpdatePeriodic) * time.Second * 60,
				Execute:  func() error { return c.runPeriodic(c.certMonitor) },
			}})
	}

	// Start periodic tasks
	for i := range c.tasks {
		c.logger.Printf("Start %s periodic task", c.tasks[i].tag)
		if err := c.tasks[i].Start(); err != nil {
			return fmt.Errorf("start %s periodic task: %w", c.tasks[i].tag, err)
		}
	}

	return nil
}

// Close implement the Close() function of the service interface
func (c *Controller) Close() error {
	c.taskState.Lock()
	c.closing.Store(true)
	c.taskState.Unlock()

	var closeErrs []error
	for i := range c.tasks {
		if c.tasks[i].Periodic != nil {
			if err := c.tasks[i].Periodic.Close(); err != nil {
				closeErrs = append(closeErrs, fmt.Errorf("%s periodic task: %w", c.tasks[i].tag, err))
			}
		}
	}
	c.taskWG.Wait()
	c.tasks = nil
	if closer, ok := c.apiClient.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			closeErrs = append(closeErrs, err)
		}
	}
	return errors.Join(closeErrs...)
}

func (c *Controller) runPeriodic(execute func() error) error {
	c.taskState.Lock()
	if c.closing.Load() {
		c.taskState.Unlock()
		return nil
	}
	c.taskWG.Add(1)
	c.taskState.Unlock()
	defer c.taskWG.Done()

	// Node, user and certificate refreshes all mutate controller/Core state.
	// Execute them serially so a slow panel request cannot overlap another task.
	c.monitorAccess.Lock()
	defer c.monitorAccess.Unlock()
	if c.closing.Load() {
		return nil
	}
	return execute()
}

func (c *Controller) nodeInfoMonitor() (err error) {
	// delay to start
	if time.Since(c.startAt) < time.Duration(c.config.UpdatePeriodic)*time.Second {
		return nil
	}

	// First fetch Node Info
	var nodeInfoChanged = true
	newNodeInfo, err := c.apiClient.GetNodeInfo()
	if err != nil {
		if err.Error() == api.NodeNotModified {
			nodeInfoChanged = false
			newNodeInfo = c.nodeInfo
		} else {
			c.logger.Print(err)
			return nil
		}
	}
	if newNodeInfo.Port == 0 {
		return errors.New("server port must > 0")
	}

	// Update User
	var usersChanged = true
	newUserInfo, err := c.apiClient.GetUserList()
	if err != nil {
		if err.Error() == api.UserNotModified {
			usersChanged = false
			newUserInfo = c.userList
		} else {
			c.logger.Print(err)
			return nil
		}
	}
	var replacedOldTag string
	var replacedOldNodeInfo *api.NodeInfo

	// If nodeInfo changed
	if nodeInfoChanged {
		if !reflect.DeepEqual(c.nodeInfo, newNodeInfo) {
			// Remove old tag
			oldTag := c.Tag
			oldNodeInfo := c.nodeInfo
			// Flush and unregister all counters while the old tag is still active.
			if c.userList != nil {
				c.reportDeletedUserTraffic(*c.userList)
			}
			err := c.removeNodeHandlers(oldTag, oldNodeInfo)
			if err != nil {
				c.logger.Printf("old node handlers reported close errors: %s", err)
			}
			// Add new tag
			c.nodeInfo = newNodeInfo
			c.Tag = c.buildNodeTag()
			err = c.addNewTag(newNodeInfo)
			if err != nil {
				failedTag := c.Tag
				c.nodeInfo = oldNodeInfo
				c.Tag = oldTag
				if rollbackErr := c.addNewTag(oldNodeInfo); rollbackErr != nil {
					c.logger.Printf("add new tag %s failed: %s; rollback %s failed: %s", failedTag, err, oldTag, rollbackErr)
				} else if c.userList != nil {
					if rollbackErr := c.addNewUser(c.userList, oldNodeInfo); rollbackErr != nil {
						c.logger.Printf("restore users for %s failed: %s", oldTag, rollbackErr)
					}
				}
				return nil
			}
			nodeInfoChanged = true
			replacedOldTag = oldTag
			replacedOldNodeInfo = oldNodeInfo
		} else {
			nodeInfoChanged = false
		}
	}

	if nodeInfoChanged {
		err = c.addNewUser(newUserInfo, newNodeInfo)
		if err != nil {
			c.rollbackNodeReplacement(replacedOldTag, replacedOldNodeInfo, newNodeInfo, err)
			return nil
		}

		// Add Limiter
		if err := c.AddInboundLimiter(c.Tag, newNodeInfo.SpeedLimit, newUserInfo, c.config.GlobalDeviceLimitConfig); err != nil {
			c.rollbackNodeReplacement(replacedOldTag, replacedOldNodeInfo, newNodeInfo, err)
			return nil
		}
		if replacedOldTag != "" && replacedOldTag != c.Tag {
			c.DeleteRule(replacedOldTag)
			if err := c.DeleteInboundLimiter(replacedOldTag); err != nil {
				c.logger.Print(err)
			}
		}

	} else {
		var deleted, added []api.UserInfo
		if usersChanged {
			deleted, added = compareUserList(c.userList, newUserInfo)
			if len(deleted) > 0 {
				// 1. Block deleted users in the limiter first, so any connection
				//    established before removal is rejected on its next dispatch
				//    and stops consuming traffic (fixes the "ghost active user"
				//    that keeps running for hours and later dumps one huge report).
				if err := c.DeleteInboundLimiterUsers(c.Tag, deleted); err != nil {
					c.logger.Print(err)
				}
				// 2. Remove from proxy auth (blocks new authentications).
				deletedEmail := make([]string, len(deleted))
				for i, u := range deleted {
					deletedEmail[i] = fmt.Sprintf("%s|%s|%d", c.Tag, u.Email, u.UID)
				}
				if err := c.removeUsers(deletedEmail, c.Tag); err != nil {
					c.logger.Print(err)
				}
				// 3. Flush residual traffic: report what accumulated since the last
				//    report, reset, and unregister the counters.
				c.reportDeletedUserTraffic(deleted)
			}
			if len(added) > 0 {
				err = c.addNewUser(&added, c.nodeInfo)
				if err != nil {
					c.logger.Print(err)
				}
				// Update Limiter
				if err := c.UpdateInboundLimiter(c.Tag, &added); err != nil {
					c.logger.Print(err)
				}
			}
		}
		if len(deleted) > 0 || len(added) > 0 {
			c.logger.Printf("%d user deleted, %d user added", len(deleted), len(added))
		}
	}

	// Fetch rules only after a node replacement has committed, so rollback
	// never leaves the restored tag with rules from the failed configuration.
	if !c.config.DisableGetRule {
		if ruleList, err := c.apiClient.GetNodeRule(); err != nil {
			if err.Error() != api.RuleNotModified {
				c.logger.Printf("Get rule list filed: %s", err)
			}
		} else if ruleList == nil {
			c.logger.Print("panel returned a nil rule list")
		} else if err := c.UpdateRule(c.Tag, *ruleList); err != nil {
			c.logger.Print(err)
		}
	}
	c.userList = newUserInfo
	return nil
}

func (c *Controller) removeNodeHandlers(tag string, nodeInfo *api.NodeInfo) error {
	var removeErrs []error
	if err := c.removeOldTag(tag); err != nil {
		removeErrs = append(removeErrs, err)
	}
	if nodeInfo != nil && nodeInfo.NodeType == "Shadowsocks-Plugin" {
		if err := c.removeOldTag(fmt.Sprintf("dokodemo-door_%s+1", tag)); err != nil {
			removeErrs = append(removeErrs, err)
		}
	}
	return errors.Join(removeErrs...)
}

func (c *Controller) rollbackNodeReplacement(oldTag string, oldNodeInfo, failedNodeInfo *api.NodeInfo, cause error) {
	failedTag := c.Tag
	_ = c.removeNodeHandlers(failedTag, failedNodeInfo)
	c.nodeInfo = oldNodeInfo
	c.Tag = oldTag
	if rollbackErr := c.addNewTag(oldNodeInfo); rollbackErr != nil {
		c.logger.Printf("node replacement %s failed: %s; rollback %s failed: %s", failedTag, cause, oldTag, rollbackErr)
		return
	}
	if c.userList != nil {
		if rollbackErr := c.addNewUser(c.userList, oldNodeInfo); rollbackErr != nil {
			c.logger.Printf("restore users for %s failed: %s", oldTag, rollbackErr)
		}
	}
}

func (c *Controller) removeOldTag(oldTag string) (err error) {
	inboundErr := c.removeInbound(oldTag)
	outboundErr := c.removeOutbound(oldTag)
	return errors.Join(inboundErr, outboundErr)
}

func (c *Controller) addNewTag(newNodeInfo *api.NodeInfo) (err error) {
	if newNodeInfo.NodeType != "Shadowsocks-Plugin" {
		inboundConfig, err := InboundBuilder(c.config, newNodeInfo, c.Tag)
		if err != nil {
			return err
		}
		err = c.addInbound(inboundConfig)
		if err != nil {

			return err
		}
		outBoundConfig, err := OutboundBuilder(c.config, newNodeInfo, c.Tag)
		if err != nil {
			_ = c.removeInbound(c.Tag)
			return err
		}
		err = c.addOutbound(outBoundConfig)
		if err != nil {
			_ = c.removeInbound(c.Tag)
			return err
		}

	} else {
		return c.addInboundForSSPlugin(*newNodeInfo)
	}
	return nil
}

func (c *Controller) addInboundForSSPlugin(newNodeInfo api.NodeInfo) (err error) {
	dokodemoTag := fmt.Sprintf("dokodemo-door_%s+1", c.Tag)
	baseInboundAdded, baseOutboundAdded := false, false
	dokodemoInboundAdded, dokodemoOutboundAdded := false, false
	defer func() {
		if err == nil {
			return
		}
		if dokodemoOutboundAdded {
			_ = c.removeOutbound(dokodemoTag)
		}
		if dokodemoInboundAdded {
			_ = c.removeInbound(dokodemoTag)
		}
		if baseOutboundAdded {
			_ = c.removeOutbound(c.Tag)
		}
		if baseInboundAdded {
			_ = c.removeInbound(c.Tag)
		}
	}()
	// Shadowsocks-Plugin require a separate inbound for other TransportProtocol likes: ws, grpc
	fakeNodeInfo := newNodeInfo
	fakeNodeInfo.TransportProtocol = "tcp"
	fakeNodeInfo.EnableTLS = false
	// Add a regular Shadowsocks inbound and outbound
	inboundConfig, err := InboundBuilder(c.config, &fakeNodeInfo, c.Tag)
	if err != nil {
		return err
	}
	err = c.addInbound(inboundConfig)
	if err != nil {

		return err
	}
	baseInboundAdded = true
	outBoundConfig, err := OutboundBuilder(c.config, &fakeNodeInfo, c.Tag)
	if err != nil {

		return err
	}
	err = c.addOutbound(outBoundConfig)
	if err != nil {

		return err
	}
	baseOutboundAdded = true
	// Add an inbound for upper streaming protocol
	fakeNodeInfo = newNodeInfo
	fakeNodeInfo.Port++
	fakeNodeInfo.NodeType = "dokodemo-door"
	inboundConfig, err = InboundBuilder(c.config, &fakeNodeInfo, dokodemoTag)
	if err != nil {
		return err
	}
	err = c.addInbound(inboundConfig)
	if err != nil {

		return err
	}
	dokodemoInboundAdded = true
	outBoundConfig, err = OutboundBuilder(c.config, &fakeNodeInfo, dokodemoTag)
	if err != nil {

		return err
	}
	err = c.addOutbound(outBoundConfig)
	if err != nil {

		return err
	}
	dokodemoOutboundAdded = true
	return nil
}

func (c *Controller) addNewUser(userInfo *[]api.UserInfo, nodeInfo *api.NodeInfo) (err error) {
	users := make([]*protocol.User, 0)
	switch nodeInfo.NodeType {
	case "V2ray", "Vmess", "Vless":
		if nodeInfo.EnableVless || (nodeInfo.NodeType == "Vless" && nodeInfo.NodeType != "Vmess") {
			users = c.buildVlessUser(userInfo)
		} else {
			users = c.buildVmessUser(userInfo)
		}
	case "Trojan":
		users = c.buildTrojanUser(userInfo)
	case "Shadowsocks":
		users = c.buildSSUser(userInfo, nodeInfo.CypherMethod)
	case "Shadowsocks-Plugin":
		users = c.buildSSPluginUser(userInfo)
	case "Hysteria":
		users = c.buildHysteriaUser(userInfo)
	default:
		return fmt.Errorf("unsupported node type: %s", nodeInfo.NodeType)
	}

	err = c.addUsers(users, c.Tag)
	if err != nil {
		return err
	}
	c.logger.Printf("Added %d new users", len(*userInfo))
	return nil
}

func compareUserList(old, new *[]api.UserInfo) (deleted, added []api.UserInfo) {
	mSrc := make(map[api.UserInfo]byte) // 按源数组建索引
	mAll := make(map[api.UserInfo]byte) // 源+目所有元素建索引

	var set []api.UserInfo // 交集

	// 1.源数组建立map
	for _, v := range *old {
		mSrc[v] = 0
		mAll[v] = 0
	}
	// 2.目数组中，存不进去，即重复元素，所有存不进去的集合就是并集
	for _, v := range *new {
		l := len(mAll)
		mAll[v] = 1
		if l != len(mAll) { // 长度变化，即可以存
			l = len(mAll)
		} else { // 存不了，进并集
			set = append(set, v)
		}
	}
	// 3.遍历交集，在并集中找，找到就从并集中删，删完后就是补集（即并-交=所有变化的元素）
	for _, v := range set {
		delete(mAll, v)
	}
	// 4.此时，mall是补集，所有元素去源中找，找到就是删除的，找不到的必定能在目数组中找到，即新加的
	for v := range mAll {
		_, exist := mSrc[v]
		if exist {
			deleted = append(deleted, v)
		} else {
			added = append(added, v)
		}
	}

	return deleted, added
}

func limitUser(c *Controller, user api.UserInfo, silentUsers *[]api.UserInfo) {
	c.limitedUsers[user] = LimitInfo{
		end:               time.Now().Unix() + int64(c.config.AutoSpeedLimitConfig.LimitDuration*60),
		currentSpeedLimit: c.config.AutoSpeedLimitConfig.LimitSpeed,
		originSpeedLimit:  user.SpeedLimit,
	}
	c.logger.Printf("Limit User: %s Speed: %d End: %s", c.buildUserTag(&user), c.config.AutoSpeedLimitConfig.LimitSpeed, time.Unix(c.limitedUsers[user].end, 0).Format("01-02 15:04:05"))
	user.SpeedLimit = uint64((c.config.AutoSpeedLimitConfig.LimitSpeed * 1000000) / 8)
	*silentUsers = append(*silentUsers, user)
}

// reportDeletedUserTraffic flushes the residual traffic counters of users that
// are being removed: it reports whatever they accumulated since the last report,
// resets the counters on success, and unregisters them so a re-added user cannot
// resurface stale accumulation as one huge delta.
func (c *Controller) reportDeletedUserTraffic(deleted []api.UserInfo) {
	var userTraffic []api.UserTraffic
	var upCounterList []stats.Counter
	var downCounterList []stats.Counter
	for i := range deleted {
		up, down, upCounter, downCounter := c.getTraffic(c.buildUserTag(&deleted[i]))
		if up > 0 || down > 0 {
			userTraffic = append(userTraffic, api.UserTraffic{
				UID:      deleted[i].UID,
				Email:    deleted[i].Email,
				Upload:   up,
				Download: down,
			})
			if upCounter != nil {
				upCounterList = append(upCounterList, upCounter)
			}
			if downCounter != nil {
				downCounterList = append(downCounterList, downCounter)
			}
		}
	}
	if len(userTraffic) > 0 && !c.config.DisableUploadTraffic {
		if err := c.apiClient.ReportUserTraffic(&userTraffic); err != nil {
			c.logger.Print(err)
		} else {
			c.resetTraffic(&upCounterList, &downCounterList)
		}
	}
	// Unregister counters regardless, so the stats manager keeps no stale counter
	// that a re-added user would later read as one huge delta.
	for i := range deleted {
		c.unregisterTraffic(c.buildUserTag(&deleted[i]))
	}
}

func (c *Controller) userInfoMonitor() (err error) {
	// delay to start
	if time.Since(c.startAt) < time.Duration(c.config.UpdatePeriodic)*time.Second {
		return nil
	}

	// Get server status
	CPU, Mem, Disk, Uptime, err := serverstatus.GetSystemInfo()
	if err != nil {
		c.logger.Print(err)
	}
	err = c.apiClient.ReportNodeStatus(
		&api.NodeStatus{
			CPU:    CPU,
			Mem:    Mem,
			Disk:   Disk,
			Uptime: Uptime,
		})
	if err != nil {
		c.logger.Print(err)
	}
	// Unlock users
	if c.config.AutoSpeedLimitConfig.Limit > 0 && len(c.limitedUsers) > 0 {
		c.logger.Printf("Limited users:")
		toReleaseUsers := make([]api.UserInfo, 0)
		for user, limitInfo := range c.limitedUsers {
			if time.Now().Unix() > limitInfo.end {
				user.SpeedLimit = limitInfo.originSpeedLimit
				toReleaseUsers = append(toReleaseUsers, user)
				c.logger.Printf("User: %s Speed: %d End: nil (Unlimit)", c.buildUserTag(&user), user.SpeedLimit)
				delete(c.limitedUsers, user)
			} else {
				c.logger.Printf("User: %s Speed: %d End: %s", c.buildUserTag(&user), limitInfo.currentSpeedLimit, time.Unix(c.limitedUsers[user].end, 0).Format("01-02 15:04:05"))
			}
		}
		if len(toReleaseUsers) > 0 {
			if err := c.UpdateInboundLimiter(c.Tag, &toReleaseUsers); err != nil {
				c.logger.Print(err)
			}
		}
	}

	// Get User traffic
	var userTraffic []api.UserTraffic
	var upCounterList []stats.Counter
	var downCounterList []stats.Counter
	AutoSpeedLimit := int64(c.config.AutoSpeedLimitConfig.Limit)
	UpdatePeriodic := int64(c.config.UpdatePeriodic)
	limitedUsers := make([]api.UserInfo, 0)
	for _, user := range *c.userList {
		up, down, upCounter, downCounter := c.getTraffic(c.buildUserTag(&user))
		if up > 0 || down > 0 {
			// Over speed users
			if AutoSpeedLimit > 0 {
				if down > AutoSpeedLimit*1000000*UpdatePeriodic/8 || up > AutoSpeedLimit*1000000*UpdatePeriodic/8 {
					if _, ok := c.limitedUsers[user]; !ok {
						if c.config.AutoSpeedLimitConfig.WarnTimes == 0 {
							limitUser(c, user, &limitedUsers)
						} else {
							c.warnedUsers[user] += 1
							if c.warnedUsers[user] > c.config.AutoSpeedLimitConfig.WarnTimes {
								limitUser(c, user, &limitedUsers)
								delete(c.warnedUsers, user)
							}
						}
					}
				} else {
					delete(c.warnedUsers, user)
				}
			}
			userTraffic = append(userTraffic, api.UserTraffic{
				UID:      user.UID,
				Email:    user.Email,
				Upload:   up,
				Download: down})

			if upCounter != nil {
				upCounterList = append(upCounterList, upCounter)
			}
			if downCounter != nil {
				downCounterList = append(downCounterList, downCounter)
			}
		} else {
			delete(c.warnedUsers, user)
		}
	}
	if len(limitedUsers) > 0 {
		if err := c.UpdateInboundLimiter(c.Tag, &limitedUsers); err != nil {
			c.logger.Print(err)
		}
	}
	if len(userTraffic) > 0 {
		var err error // Define an empty error
		if !c.config.DisableUploadTraffic {
			err = c.apiClient.ReportUserTraffic(&userTraffic)
		}
		// If report traffic error, not clear the traffic
		if err != nil {
			c.logger.Print(err)
		} else {
			c.resetTraffic(&upCounterList, &downCounterList)
		}
	}

	// Report Online info
	if onlineDevice, err := c.GetOnlineDevice(c.Tag); err != nil {
		c.logger.Print(err)
	} else if len(*onlineDevice) > 0 {
		if err = c.apiClient.ReportNodeOnlineUsers(onlineDevice); err != nil {
			c.logger.Print(err)
		} else {
			if time.Since(c.lastOnlineLog) >= time.Hour {
				c.lastOnlineLog = time.Now()
				c.logger.Printf("Report %d online users", len(*onlineDevice))
			}
		}
	}

	// Report Illegal user
	if detectResult, err := c.GetDetectResult(c.Tag); err != nil {
		c.logger.Print(err)
	} else if len(*detectResult) > 0 {
		if err = c.apiClient.ReportIllegal(detectResult); err != nil {
			c.logger.Print(err)
		} else {
			c.logger.Printf("Report %d illegal behaviors", len(*detectResult))
		}

	}
	return nil
}

func (c *Controller) buildNodeTag() string {
	return fmt.Sprintf("%s_%s_%d", c.nodeInfo.NodeType, c.config.ListenIP, c.nodeInfo.Port)
}

// func (c *Controller) logPrefix() string {
// 	return fmt.Sprintf("[%s] %s(ID=%d)", c.clientInfo.APIHost, c.nodeInfo.NodeType, c.nodeInfo.NodeID)
// }

// Check Cert
func (c *Controller) certMonitor() error {
	if c.nodeInfo.EnableTLS && !c.config.EnableREALITY && !c.nodeInfo.EnableREALITY {
		switch c.config.CertConfig.CertMode {
		case "dns", "http", "tls":
			lego, err := mylego.New(c.config.CertConfig)
			if err != nil {
				c.logger.Print(err)
			}
			// Xray-core supports the OcspStapling certification hot renew
			_, _, _, err = lego.RenewCert()
			if err != nil {
				c.logger.Print(err)
			}
		}
	}
	return nil
}
