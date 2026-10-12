package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/inbound"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/features/stats"
	"github.com/xtls/xray-core/proxy"
	"google.golang.org/protobuf/proto"

	"github.com/XrayR-project/XrayR/api"
	"github.com/XrayR-project/XrayR/common/limiter"
)

func (c *Controller) removeInbound(tag string) error {
	if err := c.ibm.RemoveHandler(context.Background(), tag); err != nil {
		return fmt.Errorf("remove inbound tag=%s: %w", tag, err)
	}
	return nil
}

func (c *Controller) removeOutbound(tag string) error {
	if err := c.obm.RemoveHandler(context.Background(), tag); err != nil {
		return fmt.Errorf("remove outbound tag=%s: %w", tag, err)
	}
	return nil
}

func (c *Controller) addInbound(config *core.InboundHandlerConfig) error {
	rawHandler, err := core.CreateObject(c.server, config)
	if err != nil {
		return err
	}
	handler, ok := rawHandler.(inbound.Handler)
	if !ok {
		return fmt.Errorf("not an InboundHandler: %s", err)
	}
	if err := c.ibm.AddHandler(context.Background(), handler); err != nil {
		return err
	}
	return nil
}

func (c *Controller) addOutbound(config *core.OutboundHandlerConfig) error {
	rawHandler, err := core.CreateObject(c.server, config)
	if err != nil {
		return err
	}
	handler, ok := rawHandler.(outbound.Handler)
	if !ok {
		return fmt.Errorf("not an InboundHandler: %s", err)
	}
	if err := c.obm.AddHandler(context.Background(), handler); err != nil {
		return err
	}
	return nil
}

func (c *Controller) userManager(tag string) (proxy.UserManager, error) {
	handler, err := c.ibm.GetHandler(context.Background(), tag)
	if err != nil {
		return nil, fmt.Errorf("get inbound %s: %w", tag, err)
	}
	inboundInstance, ok := handler.(proxy.GetInbound)
	if !ok {
		return nil, fmt.Errorf("handler %s has not implemented proxy.GetInbound", tag)
	}

	userManager, ok := inboundInstance.GetInbound().(proxy.UserManager)
	if !ok {
		return nil, fmt.Errorf("handler %s has not implemented proxy.UserManager", tag)
	}
	return userManager, nil
}

func (c *Controller) addUsers(users []*protocol.User, tag string) ([]string, error) {
	manager, err := c.userManager(tag)
	if err != nil {
		return nil, err
	}
	var added []string
	var failures []error
	for _, item := range users {
		if item == nil {
			failures = append(failures, errors.New("empty user account from user builder"))
			continue
		}
		mUser, err := item.ToMemoryUser()
		if err != nil {
			failures = append(failures, fmt.Errorf("parse user email:%s: %w", strings.TrimPrefix(item.Email, tag+"|"), err))
			continue
		}
		if existing := manager.GetUser(context.Background(), mUser.Email); existing != nil {
			// A retry must not fail on users already added successfully. Compare
			// full accounts: VLESS Account.Equals compares only the UUID, not flow.
			if existing.Level == mUser.Level && proto.Equal(existing.Account.ToProto(), mUser.Account.ToProto()) {
				added = append(added, item.Email)
				continue
			}
			if err := manager.RemoveUser(context.Background(), mUser.Email); err != nil {
				failures = append(failures, fmt.Errorf("replace user email:%s: %w", strings.TrimPrefix(item.Email, tag+"|"), err))
				continue
			}
		}
		if err := manager.AddUser(context.Background(), mUser); err != nil {
			failures = append(failures, fmt.Errorf("add user email:%s: %w", strings.TrimPrefix(item.Email, tag+"|"), err))
			continue
		}
		added = append(added, item.Email)
	}
	return added, errors.Join(failures...)
}

func (c *Controller) removeUsers(users []string, tag string) ([]string, error) {
	manager, err := c.userManager(tag)
	if err != nil {
		return nil, err
	}
	var removed []string
	var failures []error
	for _, email := range users {
		// Already absent is success, including a previous partial retry.
		if manager.GetUser(context.Background(), email) != nil {
			if err := manager.RemoveUser(context.Background(), email); err != nil {
				failures = append(failures, fmt.Errorf("remove user email:%s: %w", strings.TrimPrefix(email, tag+"|"), err))
				continue
			}
		}
		removed = append(removed, email)
	}
	return removed, errors.Join(failures...)
}

func (c *Controller) getTraffic(email string) (up int64, down int64, upCounter stats.Counter, downCounter stats.Counter) {
	upName := "user>>>" + email + ">>>traffic>>>uplink"
	downName := "user>>>" + email + ">>>traffic>>>downlink"
	upCounter = c.stm.GetCounter(upName)
	downCounter = c.stm.GetCounter(downName)
	if upCounter != nil {
		up = upCounter.Value()
	}
	if downCounter != nil {
		down = downCounter.Value()
	}
	return up, down, upCounter, downCounter
}

type trafficSnapshot struct {
	api.UserTraffic
	upCounter, downCounter stats.Counter
}

// Capture the bytes being reported so success only subtracts that amount.
// A failed report leaves all bytes available for the next reporting cycle.
func (c *Controller) snapshotTraffic(user api.UserInfo) *trafficSnapshot {
	up, down, upCounter, downCounter := c.getTraffic(c.buildUserTag(&user))
	if up <= 0 && down <= 0 {
		return nil
	}
	return &trafficSnapshot{
		UserTraffic: api.UserTraffic{UID: user.UID, Email: user.Email, Upload: up, Download: down},
		upCounter:   upCounter, downCounter: downCounter,
	}
}

func (c *Controller) reportTraffic(snapshots []*trafficSnapshot) error {
	if len(snapshots) == 0 {
		return nil
	}
	traffic := make([]api.UserTraffic, len(snapshots))
	for i, snapshot := range snapshots {
		traffic[i] = snapshot.UserTraffic
	}
	if !c.config.DisableUploadTraffic {
		if err := c.apiClient.ReportUserTraffic(&traffic); err != nil {
			return err
		}
	}
	for _, snapshot := range snapshots {
		// Subtract only acknowledged bytes; Set(0) would erase live traffic.
		if snapshot.upCounter != nil {
			snapshot.upCounter.Add(-snapshot.Upload)
		}
		if snapshot.downCounter != nil {
			snapshot.downCounter.Add(-snapshot.Download)
		}
	}
	return nil
}

func (c *Controller) AddInboundLimiter(tag string, nodeSpeedLimit uint64, userList *[]api.UserInfo, globalDeviceLimitConfig *limiter.GlobalDeviceLimitConfig) error {
	err := c.dispatcher.Limiter.AddInboundLimiter(tag, nodeSpeedLimit, userList, globalDeviceLimitConfig)
	return err
}

func (c *Controller) UpdateInboundLimiter(tag string, updatedUserList *[]api.UserInfo) error {
	err := c.dispatcher.Limiter.UpdateInboundLimiter(tag, updatedUserList)
	return err
}

func (c *Controller) DeleteInboundLimiter(tag string) error {
	err := c.dispatcher.Limiter.DeleteInboundLimiter(tag)
	return err
}

// DeleteInboundLimiterUsers blocks the given users in the limiter so any
// connection they established before removal is rejected on its next dispatch.
func (c *Controller) DeleteInboundLimiterUsers(tag string, users []api.UserInfo) error {
	return c.dispatcher.Limiter.DeleteUsers(tag, users)
}

// unregisterTraffic removes a user's traffic counters from the stats manager so
// a re-added user cannot read stale accumulation as one huge delta.
func (c *Controller) unregisterTraffic(email string) {
	upName := "user>>>" + email + ">>>traffic>>>uplink"
	downName := "user>>>" + email + ">>>traffic>>>downlink"
	if err := c.stm.UnregisterCounter(upName); err != nil {
		c.logger.Warnf("Unregister uplink counter failed email:%s: %s", strings.TrimPrefix(email, c.Tag+"|"), err)
	}
	if err := c.stm.UnregisterCounter(downName); err != nil {
		c.logger.Warnf("Unregister downlink counter failed email:%s: %s", strings.TrimPrefix(email, c.Tag+"|"), err)
	}
}

func (c *Controller) GetOnlineDevice(tag string) (*[]api.OnlineUser, error) {
	return c.dispatcher.Limiter.GetOnlineDevice(tag)
}

func (c *Controller) UpdateRule(tag string, newRuleList []api.DetectRule) error {
	err := c.dispatcher.RuleManager.UpdateRule(tag, newRuleList)
	return err
}

func (c *Controller) DeleteRule(tag string) {
	c.dispatcher.RuleManager.DeleteRule(tag)
}

func (c *Controller) GetDetectResult(tag string) (*[]api.DetectResult, error) {
	return c.dispatcher.RuleManager.GetDetectResult(tag)
}
