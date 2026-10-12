package mydispatcher

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/proxy/freedom"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/internet"
)

type lifecycleDialer struct{ internet.Dialer }

func (*lifecycleDialer) SetOutboundGateway(context.Context, *session.Outbound) {}

type lifecycleOutbound struct {
	outbound.Handler
	h       *freedom.Handler
	entered chan struct{}
}

func (*lifecycleOutbound) Tag() string { return "blocked" }
func (h *lifecycleOutbound) Dispatch(ctx context.Context, link *transport.Link) {
	close(h.entered)
	h.h.Process(ctx, link, &lifecycleDialer{})
}

type lifecycleOutbounds struct {
	outbound.Manager
	h outbound.Handler
}

func (m *lifecycleOutbounds) GetHandler(string) outbound.Handler { return m.h }

func TestLifecycleDirectBlackholeAndCancellation(t *testing.T) {
	for _, cancelEarly := range []bool{false, true} {
		t.Run(map[bool]string{false: "blackhole timeout", true: "session cancellation"}[cancelEarly], func(t *testing.T) {
			conn, peer := net.Pipe()
			defer conn.Close()
			defer peer.Close()
			h := &freedom.Handler{}
			if err := h.Init(&freedom.Config{FinalRules: []*freedom.FinalRuleConfig{{
				Action: freedom.RuleAction_Block, BlockDelay: &freedom.Range{Min: 1, Max: 1},
			}}}, nil); err != nil {
				t.Fatal(err)
			}
			handler := &lifecycleOutbound{h: h, entered: make(chan struct{})}
			d := &DefaultDispatcher{ohm: &lifecycleOutbounds{h: handler}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx = session.ContextWithInbound(ctx, &session.Inbound{Conn: conn})
			// The buf.Reader does not expose socket Close/Interrupt (like Vision).
			link := &transport.Link{Reader: buf.NewReader(conn), Writer: buf.Discard}
			done := make(chan error, 1)
			go func() { done <- d.DispatchLink(ctx, xnet.TCPDestination(xnet.LocalHostIP, 80), link) }()
			select {
			case <-handler.entered:
			case <-time.After(time.Second):
				t.Fatal("dispatch did not start")
			}
			wait := 3 * time.Second
			if cancelEarly {
				cancel()
				wait = 500 * time.Millisecond
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(wait):
				t.Fatal("blackhole/cancellation retained the direct socket read")
			}
		})
	}
}
