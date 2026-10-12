package controller

import (
	"context"
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
	server          *core.Instance
	config          *Config
	clientInfo      api.ClientInfo
	apiClient       api.API
	nodeInfo        *api.NodeInfo
	Tag             string
	userList        *[]api.UserInfo
	pendingNodeInfo *api.NodeInfo
	pendingUserList *[]api.UserInfo // Only the latest desired snapshot, never a retry queue.
	tasks           []periodicTask
	limitedUsers    map[api.UserInfo]LimitInfo
	warnedUsers     map[api.UserInfo]int
	panelType       string
	ibm             inbound.Manager
	obm             outbound.Manager
	stm             stats.Manager
	dispatcher      *mydispatcher.DefaultDispatcher
	startAt         time.Time
	lastOnlineLog   time.Time // throttle: last time the online-user count was logged
	logger          *log.Entry
	monitorAccess   sync.Mutex
	taskState       sync.Mutex
	taskWG          sync.WaitGroup
	closing         atomic.Bool
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
		c.logger.Errorf("Fetch initial node info failed: %s", err)
		return err
	}
	if newNodeInfo == nil || newNodeInfo.Port == 0 {
		c.logger.Error("Invalid initial node info: server port must > 0")
		return errors.New("server port must > 0")
	}
	c.nodeInfo = newNodeInfo
	c.Tag = c.buildNodeTag()

	// Add new tag
	err = c.addNewTag(newNodeInfo)
	if err != nil {
		c.logger.Errorf("Create initial node handlers failed: %s", err)
		return err
	}
	// Update user
	userInfo, err := c.apiClient.GetUserList()
	if err != nil {
		c.logger.Errorf("Fetch initial user list failed: %s", err)
		return err
	}
	if userInfo == nil {
		c.logger.Error("Fetch initial user list failed: panel returned a nil list")
		return errors.New("panel returned a nil user list")
	}

	// sync controller userList
	c.userList = userInfo

	_, err = c.addNewUser(userInfo, newNodeInfo)
	if err != nil {
		c.logger.Errorf("Add initial users failed: %s", err)
		return err
	}

	// Add Limiter
	if err := c.AddInboundLimiter(c.Tag, newNodeInfo.SpeedLimit, userInfo, c.config.GlobalDeviceLimitConfig); err != nil {
		c.logger.Errorf("Create initial inbound limiter failed: %s", err)
		return err
	}

	// Add Rule Manager
	if !c.config.DisableGetRule {
		if ruleList, err := c.apiClient.GetNodeRule(); err != nil {
			c.logger.Warnf("Fetch initial rule list failed: %s", err)
		} else {
			if ruleList == nil {
				c.logger.Error("Fetch initial rule list failed: panel returned a nil list")
				return errors.New("panel returned a nil rule list")
			}
			if err := c.UpdateRule(c.Tag, *ruleList); err != nil {
				c.logger.Warnf("Install initial rules failed: %s", err)
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
			c.logger.Errorf("Start %s periodic task failed: %s", c.tasks[i].tag, err)
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
				c.logger.Warnf("Close %s periodic task failed: %s", c.tasks[i].tag, err)
				closeErrs = append(closeErrs, fmt.Errorf("%s periodic task: %w", c.tasks[i].tag, err))
			}
		}
	}
	c.taskWG.Wait()
	c.tasks = nil
	if closer, ok := c.apiClient.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			c.logger.Warnf("Close panel API client failed: %s", err)
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
	if err := execute(); err != nil {
		// Periodic stops scheduling when Execute returns an error. Recoverable
		// refresh failures must leave the monitor alive for the next cycle.
		c.logger.Warnf("Periodic refresh failed; will retry next cycle: %s", err)
	}
	return nil
}

func (c *Controller) nodeInfoMonitor() (err error) {
	// delay to start
	if time.Since(c.startAt) < time.Duration(c.config.UpdatePeriodic)*time.Second {
		return nil
	}

	// First fetch Node Info
	newNodeInfo, err := c.apiClient.GetNodeInfo()
	if err != nil {
		if err.Error() == api.NodeNotModified {
			newNodeInfo = c.pendingNodeInfo
			if newNodeInfo == nil {
				newNodeInfo = c.nodeInfo
			}
		} else {
			c.logger.Warnf("Fetch node info failed; keeping current state: %s", err)
			return nil
		}
	}
	if newNodeInfo == nil || newNodeInfo.Port == 0 {
		c.logger.Warn("Invalid node info; keeping current state: server port must > 0")
		return nil
	}
	c.pendingNodeInfo = newNodeInfo
	nodeInfoChanged := !reflect.DeepEqual(c.nodeInfo, newNodeInfo)

	// Update User
	var usersChanged = true
	retryPending := c.pendingUserList != nil
	newUserInfo, err := c.apiClient.GetUserList()
	if err != nil {
		if err.Error() == api.UserNotModified {
			newUserInfo = c.pendingUserList
			if newUserInfo == nil {
				usersChanged = false
				newUserInfo = c.userList
			}
		} else {
			c.logger.Warnf("Fetch user list failed; keeping current users: %s", err)
			return nil
		}
	}
	if newUserInfo == nil {
		c.logger.Warn("Fetch user list failed; panel returned a nil list")
		return nil
	}
	if usersChanged {
		c.pendingUserList = newUserInfo
		c.pruneUserLimits(newUserInfo)
	}
	var replacedOldTag string
	var replacedOldNodeInfo *api.NodeInfo

	// If nodeInfo changed
	if nodeInfoChanged {
		// Remove old tag
		oldTag := c.Tag
		oldNodeInfo := c.nodeInfo
		// Flush and unregister all counters while the old tag is still active.
		if c.userList != nil {
			c.reportDeletedUserTraffic(*c.userList)
		}
		err := c.removeNodeHandlers(oldTag, oldNodeInfo)
		if err != nil {
			c.logger.Warnf("Remove old node handlers failed tag=%s: %s", oldTag, err)
		}
		// Add new tag
		c.nodeInfo = newNodeInfo
		c.Tag = c.buildNodeTag()
		err = c.addNewTag(newNodeInfo)
		if err != nil {
			failedTag := c.Tag
			c.logger.Warnf("Create replacement node failed tag=%s; will retry next cycle: %s", failedTag, err)
			c.nodeInfo = oldNodeInfo
			c.Tag = oldTag
			if rollbackErr := c.addNewTag(oldNodeInfo); rollbackErr != nil {
				c.logger.Errorf("Restore node failed tag=%s: %s", oldTag, rollbackErr)
			} else if c.userList != nil {
				if _, rollbackErr := c.addNewUser(c.userList, oldNodeInfo); rollbackErr != nil {
					c.logger.Errorf("Restore node users failed tag=%s: %s", oldTag, rollbackErr)
				}
			}
			return nil
		}
		replacedOldTag = oldTag
		replacedOldNodeInfo = oldNodeInfo
	}

	if nodeInfoChanged {
		_, err = c.addNewUser(newUserInfo, newNodeInfo)
		if err != nil {
			c.rollbackNodeReplacement(replacedOldTag, replacedOldNodeInfo, newNodeInfo, err)
			return nil
		}

		// Add Limiter
		limiterUsers := c.usersWithActiveLimits(*newUserInfo)
		if err := c.AddInboundLimiter(c.Tag, newNodeInfo.SpeedLimit, &limiterUsers, c.config.GlobalDeviceLimitConfig); err != nil {
			c.rollbackNodeReplacement(replacedOldTag, replacedOldNodeInfo, newNodeInfo, err)
			return nil
		}
		if replacedOldTag != "" && replacedOldTag != c.Tag {
			c.DeleteRule(replacedOldTag)
			if err := c.DeleteInboundLimiter(replacedOldTag); err != nil {
				c.logger.Warnf("Delete old inbound limiter failed tag=%s: %s", replacedOldTag, err)
			}
		}

		c.userList = newUserInfo
	} else if usersChanged {
		if err := c.syncUserList(newUserInfo, retryPending); err != nil {
			// Successful operations have been committed individually; retain only
			// the latest target so HTTP 304 still retries unfinished operations.
			c.pruneUserWarnings(c.userList)
			return nil
		}
	}
	c.pendingNodeInfo = nil
	c.pendingUserList = nil

	// Fetch rules only after a node replacement has committed, so rollback
	// never leaves the restored tag with rules from the failed configuration.
	if !c.config.DisableGetRule {
		if ruleList, err := c.apiClient.GetNodeRule(); err != nil {
			if err.Error() != api.RuleNotModified {
				c.logger.Warnf("Fetch rule list failed; keeping current rules: %s", err)
			}
		} else if ruleList == nil {
			c.logger.Warn("Fetch rule list failed; panel returned a nil list")
		} else if err := c.UpdateRule(c.Tag, *ruleList); err != nil {
			c.logger.Warnf("Update rules failed: %s", err)
		}
	}
	if usersChanged {
		c.pruneUserWarnings(newUserInfo)
	}
	return nil
}

func (c *Controller) syncUserList(desired *[]api.UserInfo, retry bool) error {
	deleted, added := compareUserList(c.userList, desired)
	if len(deleted) == 0 && len(added) == 0 && !retry {
		c.userList = desired
		return nil
	}
	removed := removedUsers(deleted, added)
	if len(removed) > 0 {
		if err := c.DeleteInboundLimiterUsers(c.Tag, removed); err != nil {
			c.logger.Warnf("Block removed users failed users=%d; will retry next cycle: %s", len(removed), err)
			return err
		}
	}
	applied := make(map[string]api.UserInfo, len(*c.userList)+len(added))
	for _, user := range *c.userList {
		applied[c.buildUserTag(&user)] = user
	}
	var failures []error
	removedOK := make(map[string]bool, len(deleted))
	if len(deleted) > 0 {
		emails := make([]string, len(deleted))
		for i := range deleted {
			emails[i] = c.buildUserTag(&deleted[i])
		}
		done, err := c.removeUsers(emails, c.Tag)
		for _, email := range done {
			removedOK[email] = true
		}
		if err != nil {
			c.logger.Warnf("Remove users failed completed=%d requested=%d; will retry next cycle: %s", len(done), len(deleted), err)
			failures = append(failures, err)
		}
	}
	var flushed []api.UserInfo
	for _, user := range removed {
		if email := c.buildUserTag(&user); removedOK[email] {
			delete(applied, email)
			flushed = append(flushed, user)
		}
	}
	c.reportDeletedUserTraffic(flushed)
	// Never add a replacement while removal of that same identity failed.
	toAdd := make([]api.UserInfo, 0, len(added))
	for _, user := range added {
		email := c.buildUserTag(&user)
		if _, exists := applied[email]; !exists || removedOK[email] {
			toAdd = append(toAdd, user)
		}
	}
	if retry {
		// A failed parameter replacement can remove auth while keeping the old
		// list entry as the owner of its traffic counters. If the panel reverts
		// to that old entry, restore missing auth even though the list diff is empty.
		manager, err := c.userManager(c.Tag)
		if err != nil {
			c.logger.Warnf("Inspect auth users for retry failed; will retry next cycle: %s", err)
			failures = append(failures, err)
		} else {
			for _, user := range *desired {
				email := c.buildUserTag(&user)
				if current, exists := applied[email]; exists && current == user && manager.GetUser(context.Background(), email) == nil {
					toAdd = append(toAdd, user)
				}
			}
		}
	}
	var addedOK []api.UserInfo
	if len(toAdd) > 0 {
		var err error
		addedOK, err = c.addNewUser(&toAdd, c.nodeInfo)
		for _, user := range addedOK {
			applied[c.buildUserTag(&user)] = user
		}
		if err != nil {
			c.logger.Warnf("Add users failed completed=%d requested=%d; will retry next cycle: %s", len(addedOK), len(toAdd), err)
			failures = append(failures, err)
		}
	}
	// Keep old parameter variants until their replacement succeeds. If the
	// target later drops such a user, their preserved counters still get flushed.
	current := make([]api.UserInfo, 0, len(applied))
	for _, user := range applied {
		current = append(current, user)
	}
	c.userList = &current
	limiterUsers := addedOK
	if retry {
		// Auth may already have succeeded when a previous limiter update failed.
		limiterUsers = make([]api.UserInfo, 0, len(*desired))
		for _, user := range *desired {
			if current, exists := applied[c.buildUserTag(&user)]; exists && current == user {
				limiterUsers = append(limiterUsers, user)
			}
		}
	}
	if len(limiterUsers) > 0 {
		limiterUsers = c.usersWithActiveLimits(limiterUsers)
		if err := c.UpdateInboundLimiter(c.Tag, &limiterUsers); err != nil {
			c.logger.Warnf("Update user limiter failed users=%d; will retry next cycle: %s", len(limiterUsers), err)
			failures = append(failures, err)
		}
	}
	if len(removedOK) > 0 || len(addedOK) > 0 {
		c.logger.Infof("%d user deleted, %d user added", len(removedOK), len(addedOK))
	}
	if err := errors.Join(failures...); err != nil {
		return err
	}
	c.userList = desired
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
	c.logger.Warnf("Node replacement failed tag=%s; will retry next cycle: %s", failedTag, cause)
	if err := c.removeNodeHandlers(failedTag, failedNodeInfo); err != nil {
		c.logger.Warnf("Remove failed replacement handlers failed tag=%s: %s", failedTag, err)
	}
	c.nodeInfo = oldNodeInfo
	c.Tag = oldTag
	if rollbackErr := c.addNewTag(oldNodeInfo); rollbackErr != nil {
		c.logger.Errorf("Restore node failed tag=%s: %s", oldTag, rollbackErr)
		return
	}
	if c.userList != nil {
		if _, rollbackErr := c.addNewUser(c.userList, oldNodeInfo); rollbackErr != nil {
			c.logger.Errorf("Restore node users failed tag=%s: %s", oldTag, rollbackErr)
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
			return errors.Join(err, c.removeInbound(c.Tag))
		}
		err = c.addOutbound(outBoundConfig)
		if err != nil {
			return errors.Join(err, c.removeInbound(c.Tag))
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
			err = errors.Join(err, c.removeOutbound(dokodemoTag))
		}
		if dokodemoInboundAdded {
			err = errors.Join(err, c.removeInbound(dokodemoTag))
		}
		if baseOutboundAdded {
			err = errors.Join(err, c.removeOutbound(c.Tag))
		}
		if baseInboundAdded {
			err = errors.Join(err, c.removeInbound(c.Tag))
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

func (c *Controller) addNewUser(userInfo *[]api.UserInfo, nodeInfo *api.NodeInfo) ([]api.UserInfo, error) {
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
		return nil, fmt.Errorf("unsupported node type: %s", nodeInfo.NodeType)
	}

	emails, err := c.addUsers(users, c.Tag)
	done := make(map[string]bool, len(emails))
	for _, email := range emails {
		done[email] = true
	}
	added := make([]api.UserInfo, 0, len(emails))
	for _, user := range *userInfo {
		if done[c.buildUserTag(&user)] {
			added = append(added, user)
		}
	}
	if len(added) > 0 {
		c.logger.Infof("Added %d new users", len(added))
	}
	if err == nil && len(added) != len(*userInfo) {
		err = errors.New("some user accounts could not be built")
	}
	return added, err
}

func compareUserList(old, new *[]api.UserInfo) (deleted, added []api.UserInfo) {
	// One set tracks whether each unique user is present in the new list.
	seen := make(map[api.UserInfo]bool, len(*old))
	for _, user := range *old {
		seen[user] = false
	}
	for _, user := range *new {
		if _, exists := seen[user]; !exists {
			added = append(added, user)
		}
		seen[user] = true
	}
	for user, present := range seen {
		if !present {
			deleted = append(deleted, user)
		}
	}
	return deleted, added
}

func (c *Controller) pruneUserWarnings(users *[]api.UserInfo) {
	if c.warnedUsers == nil {
		return
	}
	// Keep unchanged users' warning counts, dropping removed users and old
	// parameter/auth variants. Rebuilding also releases oversized map storage.
	active := make(map[api.UserInfo]int)
	for _, user := range *users {
		if count, exists := c.warnedUsers[user]; exists {
			active[user] = count
		}
	}
	c.warnedUsers = active
}

func (c *Controller) pruneUserLimits(users *[]api.UserInfo) {
	if len(c.limitedUsers) == 0 {
		return
	}
	active := make(map[api.UserInfo]LimitInfo)
	for _, user := range *users {
		if info, exists := c.limitedUsers[user]; exists {
			active[user] = info
		}
	}
	c.limitedUsers = active
}

func (c *Controller) usersWithActiveLimits(users []api.UserInfo) []api.UserInfo {
	if len(c.limitedUsers) == 0 {
		return users
	}
	updated := append([]api.UserInfo(nil), users...)
	for i, user := range updated {
		if info, exists := c.limitedUsers[user]; exists {
			updated[i].SpeedLimit = uint64(info.currentSpeedLimit) * 1000000 / 8
		}
	}
	return updated
}

func removedUsers(deleted, added []api.UserInfo) []api.UserInfo {
	type identity struct {
		uid   int
		email string
	}
	readded := make(map[identity]bool, len(added))
	for _, user := range added {
		readded[identity{user.UID, user.Email}] = true
	}
	removed := make([]api.UserInfo, 0, len(deleted))
	for _, user := range deleted {
		if !readded[identity{user.UID, user.Email}] {
			removed = append(removed, user)
		}
	}
	return removed
}

func limitUser(c *Controller, user api.UserInfo, silentUsers *[]api.UserInfo) {
	c.limitedUsers[user] = LimitInfo{
		end:               time.Now().Unix() + int64(c.config.AutoSpeedLimitConfig.LimitDuration*60),
		currentSpeedLimit: c.config.AutoSpeedLimitConfig.LimitSpeed,
		originSpeedLimit:  user.SpeedLimit,
	}
	user.SpeedLimit = uint64((c.config.AutoSpeedLimitConfig.LimitSpeed * 1000000) / 8)
	*silentUsers = append(*silentUsers, user)
}

// reportDeletedUserTraffic flushes the residual traffic counters of users that
// are being removed: it reports whatever they accumulated since the last report,
// subtracts acknowledged bytes, and unregisters them. Failed reports of removed
// users are deliberately discarded instead of retained for a later re-addition.
func (c *Controller) reportDeletedUserTraffic(deleted []api.UserInfo) {
	var snapshots []*trafficSnapshot
	for _, user := range deleted {
		if snapshot := c.snapshotTraffic(user); snapshot != nil {
			snapshots = append(snapshots, snapshot)
		}
	}
	if err := c.reportTraffic(snapshots); err != nil {
		c.logger.Warnf("Report removed-user traffic failed users=%d; traffic discarded and counters unregistered: %s", len(snapshots), err)
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
		c.logger.Warnf("Read system status failed: %s", err)
	}
	err = c.apiClient.ReportNodeStatus(
		&api.NodeStatus{
			CPU:    CPU,
			Mem:    Mem,
			Disk:   Disk,
			Uptime: Uptime,
		})
	if err != nil {
		c.logger.Warnf("Report node status failed: %s", err)
	}
	// Unlock users
	activeUsers := c.userList
	if c.pendingUserList != nil {
		activeUsers = c.pendingUserList
	}
	c.pruneUserLimits(activeUsers)
	if c.config.AutoSpeedLimitConfig.Limit > 0 && len(c.limitedUsers) > 0 {
		toReleaseUsers := make([]api.UserInfo, 0)
		toReleaseKeys := make([]api.UserInfo, 0)
		for user, limitInfo := range c.limitedUsers {
			if time.Now().Unix() > limitInfo.end {
				toReleaseKeys = append(toReleaseKeys, user)
				user.SpeedLimit = limitInfo.originSpeedLimit
				toReleaseUsers = append(toReleaseUsers, user)
			} else {
				c.logger.Debugf("User: %s Speed: %d End: %s", c.buildUserTag(&user), limitInfo.currentSpeedLimit, time.Unix(limitInfo.end, 0).Format("01-02 15:04:05"))
			}
		}
		if len(toReleaseUsers) > 0 {
			if err := c.UpdateInboundLimiter(c.Tag, &toReleaseUsers); err != nil {
				c.logger.Warnf("Restore user speed failed users=%d; will retry next cycle: %s", len(toReleaseUsers), err)
			} else {
				for i, user := range toReleaseKeys {
					delete(c.limitedUsers, user)
					c.logger.Infof("User: %s Speed: %d End: nil (Unlimit)", c.buildUserTag(&user), toReleaseUsers[i].SpeedLimit)
				}
			}
		}
	}

	// Get User traffic
	var userTraffic []*trafficSnapshot
	AutoSpeedLimit := int64(c.config.AutoSpeedLimitConfig.Limit)
	UpdatePeriodic := int64(c.config.UpdatePeriodic)
	limitedUsers := make([]api.UserInfo, 0)
	newLimitKeys := make([]api.UserInfo, 0)
	var desiredSet map[api.UserInfo]bool
	if c.pendingUserList != nil {
		desiredSet = make(map[api.UserInfo]bool, len(*c.pendingUserList))
		for _, user := range *c.pendingUserList {
			desiredSet[user] = true
		}
	}
	for _, user := range *c.userList {
		if snapshot := c.snapshotTraffic(user); snapshot != nil {
			up, down := snapshot.Upload, snapshot.Download
			// Over speed users
			if AutoSpeedLimit > 0 && (desiredSet == nil || desiredSet[user]) {
				if down > AutoSpeedLimit*1000000*UpdatePeriodic/8 || up > AutoSpeedLimit*1000000*UpdatePeriodic/8 {
					if _, ok := c.limitedUsers[user]; !ok {
						if c.config.AutoSpeedLimitConfig.WarnTimes == 0 {
							limitUser(c, user, &limitedUsers)
							newLimitKeys = append(newLimitKeys, user)
						} else {
							c.warnedUsers[user] += 1
							if c.warnedUsers[user] > c.config.AutoSpeedLimitConfig.WarnTimes {
								limitUser(c, user, &limitedUsers)
								newLimitKeys = append(newLimitKeys, user)
								delete(c.warnedUsers, user)
							}
						}
					}
				} else {
					delete(c.warnedUsers, user)
				}
			}
			userTraffic = append(userTraffic, snapshot)
		} else {
			delete(c.warnedUsers, user)
		}
	}
	if len(limitedUsers) > 0 {
		if err := c.UpdateInboundLimiter(c.Tag, &limitedUsers); err != nil {
			c.logger.Warnf("Apply automatic speed limit failed users=%d; will retry on next excessive traffic sample: %s", len(limitedUsers), err)
			for _, user := range newLimitKeys {
				delete(c.limitedUsers, user)
			}
		} else {
			for _, user := range newLimitKeys {
				info := c.limitedUsers[user]
				c.logger.Infof("Limit User: %s Speed: %d End: %s", c.buildUserTag(&user), info.currentSpeedLimit, time.Unix(info.end, 0).Format("01-02 15:04:05"))
			}
		}
	}
	if len(userTraffic) > 0 {
		if err := c.reportTraffic(userTraffic); err != nil {
			c.logger.Warnf("Report user traffic failed users=%d; counters retained for next cycle: %s", len(userTraffic), err)
		}
	}

	// Report Online info
	if onlineDevice, err := c.GetOnlineDevice(c.Tag); err != nil {
		c.logger.Warnf("Read online devices failed: %s", err)
	} else if len(*onlineDevice) > 0 {
		if err = c.apiClient.ReportNodeOnlineUsers(onlineDevice); err != nil {
			c.logger.Warnf("Report online users failed records=%d: %s", len(*onlineDevice), err)
		} else {
			if time.Since(c.lastOnlineLog) >= time.Hour {
				c.lastOnlineLog = time.Now()
				c.logger.Printf("Report %d online users", len(*onlineDevice))
			}
		}
	}

	// Report Illegal user
	if detectResult, err := c.GetDetectResult(c.Tag); err != nil {
		c.logger.Warnf("Read audit results failed: %s", err)
	} else if len(*detectResult) > 0 {
		if err = c.apiClient.ReportIllegal(detectResult); err != nil {
			c.logger.Warnf("Report illegal behaviors failed records=%d; drained results discarded: %s", len(*detectResult), err)
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
				c.logger.Warnf("Create certificate client failed; will retry next cycle: %s", err)
				return nil
			}
			// Xray-core supports the OcspStapling certification hot renew
			_, _, _, err = lego.RenewCert()
			if err != nil {
				c.logger.Warnf("Renew certificate failed; will retry next cycle: %s", err)
			}
		}
	}
	return nil
}
