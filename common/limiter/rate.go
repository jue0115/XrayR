package limiter

import (
	"context"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"golang.org/x/time/rate"
)

type Writer struct {
	writer  buf.Writer
	limiter *rate.Limiter
	ctx     context.Context
	cancel  context.CancelFunc
}

func (l *Limiter) RateWriter(ctx context.Context, writer buf.Writer, limiter *rate.Limiter) buf.Writer {
	ctx, cancel := context.WithCancel(ctx)
	return &Writer{
		writer:  writer,
		limiter: limiter,
		ctx:     ctx,
		cancel:  cancel,
	}
}

func (w *Writer) Close() error {
	w.cancel()
	return common.Close(w.writer)
}

func (w *Writer) Interrupt() {
	w.cancel()
	common.Interrupt(w.writer)
}

func (w *Writer) WriteMultiBuffer(mb buf.MultiBuffer) error {
	// Charge large writes in burst-sized reservations, without splitting the
	// payload (UDP datagrams must remain intact) or changing buffer settings.
	for remaining := int(mb.Len()); remaining > 0; {
		n := remaining
		if burst := w.limiter.Burst(); burst > 0 && n > burst {
			n = burst
		}
		if err := w.limiter.WaitN(w.ctx, n); err != nil {
			buf.ReleaseMulti(mb)
			return err
		}
		remaining -= n
	}
	if err := w.ctx.Err(); err != nil {
		buf.ReleaseMulti(mb)
		return err
	}
	return w.writer.WriteMultiBuffer(mb)
}
