package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/features/inbound"
	"github.com/xtls/xray-core/features/routing"
	routingSession "github.com/xtls/xray-core/features/routing/session"
)

type startupInbound struct{ start func() error }

func (*startupInbound) Tag() string                            { return "startup-check" }
func (*startupInbound) ReceiverSettings() *serial.TypedMessage { return nil }
func (*startupInbound) ProxySettings() *serial.TypedMessage    { return nil }
func (h *startupInbound) Start() error                         { return h.start() }
func (*startupInbound) Close() error                           { return nil }

func TestCoreStartsRoutingBeforeInbounds(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "route.lua")
	if err := os.WriteFile(script, []byte(`function HandleRoute() return "direct" end`), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(map[string]string{"script": script})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "route.json")
	if err := os.WriteFile(path, config, 0600); err != nil {
		t.Fatal(err)
	}
	p := New(&Config{RouteConfigPath: path})
	server := p.loadCore(p.panelConfig)
	defer server.Close()
	started := false
	handler := &startupInbound{start: func() error {
		started = true
		route, err := server.GetFeature(routing.RouterType()).(routing.Router).PickRoute(&routingSession.Context{})
		if err != nil {
			return err
		}
		if route.GetOutboundTag() != "direct" {
			return fmt.Errorf("unexpected startup route: %s", route.GetOutboundTag())
		}
		return nil
	}}
	if err := server.GetFeature(inbound.ManagerType()).(inbound.Manager).AddHandler(context.Background(), handler); err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Fatal("inbound startup check did not run")
	}
}
