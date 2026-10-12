package mydispatcher

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
)

// Track both halves: a closed upload can still have a live download (and vice
// versa). Context cancellation is a fallback for dispatch failures or shutdown.
// AfterFunc is detached on normal completion, so mux sessions do not retain a
// callback for every finished request until the parent connection closes.
func trackConnection(ctx context.Context, release func()) (uplink, downlink func()) {
	var remaining atomic.Int32
	remaining.Store(2)
	stop := context.AfterFunc(ctx, release)
	done := func() {
		if remaining.Add(-1) == 0 {
			stop()
			release()
		}
	}
	return sync.OnceFunc(done), sync.OnceFunc(done)
}

type connectionWriter struct {
	buf.Writer
	done func()
}

func (w *connectionWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	err := w.Writer.WriteMultiBuffer(mb)
	if err != nil {
		w.done()
	}
	return err
}

func (w *connectionWriter) Close() error {
	defer w.done()
	return common.Close(w.Writer)
}

func (w *connectionWriter) Interrupt() {
	defer w.done()
	common.Interrupt(w.Writer)
}

// Keep TimeoutReader available for sniffing. Reading EOF is not enough to
// release this half: sniffed/queued bytes may still be waiting to be forwarded.
// The outbound interrupts/closes its reader once processing really finishes.
type connectionTimeoutReader struct {
	buf.TimeoutReader
	done func()
}

func (r *connectionTimeoutReader) Close() error {
	defer r.done()
	return common.Close(r.TimeoutReader)
}

func (r *connectionTimeoutReader) Interrupt() {
	defer r.done()
	common.Interrupt(r.TimeoutReader)
}
