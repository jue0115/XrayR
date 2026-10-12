package mydispatcher

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/XrayR-project/XrayR/api"
	"github.com/XrayR-project/XrayR/common/limiter"
	"github.com/XrayR-project/XrayR/common/rule"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/features/policy"
	"github.com/xtls/xray-core/transport"
)

type connectionTestReader struct{ err error }

func (r *connectionTestReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	return nil, r.err
}

func (r *connectionTestReader) ReadMultiBufferTimeout(time.Duration) (buf.MultiBuffer, error) {
	return nil, r.err
}

func TestConnectionTrackingHalfCloseAndTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var released atomic.Int32
	up, down := trackConnection(ctx, sync.OnceFunc(func() { released.Add(1) }))
	base := &connectionTestReader{err: buf.ErrReadTimeout}
	reader := &connectionTimeoutReader{TimeoutReader: base, done: up}
	writer := &connectionWriter{Writer: buf.Discard, done: down}
	if _, err := reader.ReadMultiBufferTimeout(time.Millisecond); err != buf.ErrReadTimeout {
		t.Fatal("sniffing timeout changed")
	}
	writer.Close()
	writer.Interrupt()
	writer.Close()
	if released.Load() != 0 {
		t.Fatal("half-close or sniffing timeout released a live upload")
	}
	base.err = io.EOF
	reader.ReadMultiBuffer()
	if released.Load() != 0 {
		t.Fatal("EOF released a link whose buffered upload may still be forwarding")
	}
	reader.Interrupt()
	if released.Load() != 1 {
		t.Fatal("completed direct link did not release exactly once")
	}
}

func TestConnectionTrackingPipeCompletionAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "reader and writer closed", true: "context canceled"}[canceled], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			var released atomic.Int32
			up, down := trackConnection(ctx, sync.OnceFunc(func() {
				released.Add(1)
				close(done)
			}))
			uplink := &connectionTimeoutReader{TimeoutReader: &connectionTestReader{err: io.EOF}, done: up}
			downlink := &connectionWriter{Writer: buf.Discard, done: down}
			if canceled {
				cancel()
			} else {
				uplink.Interrupt()
				if released.Load() != 0 {
					t.Fatal("closed upload released a live download")
				}
				downlink.Interrupt()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("connection cleanup did not release")
			}
			uplink.Interrupt()
			downlink.Close()
			if released.Load() != 1 {
				t.Fatal("cancellation and link cleanup released more than once")
			}
		})
	}
}

type connectionTestPolicy struct{ policy.Manager }

func (*connectionTestPolicy) ForLevel(uint32) policy.Session { return policy.Session{} }

type connectionTestOutbound struct {
	outbound.Handler
	target chan string
}

func (*connectionTestOutbound) Tag() string { return "test" }

func (h *connectionTestOutbound) Dispatch(ctx context.Context, link *transport.Link) {
	target := session.OutboundsFromContext(ctx)[0].Target.Address.String()
	common.Close(link.Writer)
	common.Interrupt(link.Reader)
	h.target <- target
}

func TestConnectionTrackingKeepsPipeSniffing(t *testing.T) {
	h := &connectionTestOutbound{target: make(chan string, 1)}
	d := &DefaultDispatcher{
		ohm: &lifecycleOutbounds{h: h}, policy: &connectionTestPolicy{},
		Limiter: limiter.New(), RuleManager: rule.New(),
	}
	t.Cleanup(func() { d.Limiter.Close(); d.RuleManager.Close() })
	users := []api.UserInfo{{UID: 1, Email: "sniff@example.invalid", SpeedLimit: 1024, DeviceLimit: 1}}
	if err := d.Limiter.AddInboundLimiter("test", 0, &users, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, core.XrayKey(1), new(core.Instance))
	ctx = session.ContextWithInbound(ctx, &session.Inbound{
		Tag: "test", Source: xnet.TCPDestination(xnet.LocalHostIP, 12345),
		User: &protocol.MemoryUser{Email: "test|sniff@example.invalid|1"},
	})
	ctx = session.ContextWithContent(ctx, &session.Content{SniffingRequest: session.SniffingRequest{
		Enabled: true, OverrideDestinationForProtocol: []string{"http"},
	}})
	link, err := d.Dispatch(ctx, xnet.TCPDestination(xnet.ParseAddress("192.0.2.1"), 80))
	if err != nil {
		t.Fatal(err)
	}
	if err := link.Writer.WriteMultiBuffer(buf.MergeBytes(nil, []byte("GET / HTTP/1.1\r\nHost: sniff.example.invalid\r\n\r\n"))); err != nil {
		t.Fatal(err)
	}
	select {
	case target := <-h.target:
		if target != "sniff.example.invalid" {
			t.Fatal("tracking wrapper disabled domain sniffing:", target)
		}
	case <-time.After(time.Second):
		t.Fatal("tracked pipe did not dispatch")
	}
}
