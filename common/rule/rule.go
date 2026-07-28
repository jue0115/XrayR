// Package rule is to control the audit rule behaviors
package rule

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	mapset "github.com/deckarep/golang-set"
	"github.com/xtls/xray-core/common/errors"

	"github.com/XrayR-project/XrayR/api"
)

type Manager struct {
	InboundRule         *sync.Map // Key: Tag, Value: []api.DetectRule
	InboundDetectResult *sync.Map // key: Tag, Value: mapset.NewSet []api.DetectResult
	detectAccess        sync.Mutex
	closed              atomic.Bool
}

func New() *Manager {
	return &Manager{
		InboundRule:         new(sync.Map),
		InboundDetectResult: new(sync.Map),
	}
}

func (r *Manager) UpdateRule(tag string, newRuleList []api.DetectRule) error {
	if r.closed.Load() {
		return fmt.Errorf("rule manager is closed")
	}
	if value, ok := r.InboundRule.LoadOrStore(tag, newRuleList); ok {
		oldRuleList := value.([]api.DetectRule)
		if !reflect.DeepEqual(oldRuleList, newRuleList) {
			r.InboundRule.Store(tag, newRuleList)
		}
	}
	return nil
}

// DeleteRule removes rules and pending detection results for a retired tag.
func (r *Manager) DeleteRule(tag string) {
	if r == nil {
		return
	}
	r.detectAccess.Lock()
	defer r.detectAccess.Unlock()
	r.InboundRule.Delete(tag)
	r.InboundDetectResult.Delete(tag)
}

func (r *Manager) GetDetectResult(tag string) (*[]api.DetectResult, error) {
	r.detectAccess.Lock()
	defer r.detectAccess.Unlock()
	detectResult := make([]api.DetectResult, 0)
	if value, ok := r.InboundDetectResult.LoadAndDelete(tag); ok {
		resultSet := value.(mapset.Set)
		it := resultSet.Iterator()
		for result := range it.C {
			detectResult = append(detectResult, result.(api.DetectResult))
		}
	}
	return &detectResult, nil
}

func (r *Manager) Detect(tag string, destination string, email string) (reject bool) {
	if r.closed.Load() {
		return false
	}
	reject = false
	var hitRuleID = -1
	// If we have some rule for this inbound
	if value, ok := r.InboundRule.Load(tag); ok {
		ruleList := value.([]api.DetectRule)
		for _, r := range ruleList {
			if r.Pattern.Match([]byte(destination)) {
				hitRuleID = r.ID
				reject = true
				break
			}
		}
		// If we hit some rule
		if reject && hitRuleID != -1 {
			l := strings.Split(email, "|")
			uid, err := strconv.Atoi(l[len(l)-1])
			if err != nil {
				errors.LogDebug(context.Background(), fmt.Sprintf("Record illegal behavior failed! Cannot find user's uid: %s", email))
				return reject
			}
			r.detectAccess.Lock()
			defer r.detectAccess.Unlock()
			newSet := mapset.NewSetWith(api.DetectResult{UID: uid, RuleID: hitRuleID})
			// If there are any hit history
			if v, ok := r.InboundDetectResult.LoadOrStore(tag, newSet); ok {
				resultSet := v.(mapset.Set)
				// If this is a new record
				if resultSet.Add(api.DetectResult{UID: uid, RuleID: hitRuleID}) {
					r.InboundDetectResult.Store(tag, resultSet)
				}
			}
		}
	}
	return reject
}

// Close releases all cached rules and detection results.
func (r *Manager) Close() error {
	if r == nil || r.closed.Swap(true) {
		return nil
	}
	r.detectAccess.Lock()
	defer r.detectAccess.Unlock()
	r.InboundRule.Range(func(key, _ any) bool {
		r.InboundRule.Delete(key)
		return true
	})
	r.InboundDetectResult.Range(func(key, _ any) bool {
		r.InboundDetectResult.Delete(key)
		return true
	})
	return nil
}
