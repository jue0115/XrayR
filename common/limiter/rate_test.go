package limiter

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"golang.org/x/time/rate"
)

type rateTestWriter struct {
	writes atomic.Int32
	bytes  int32
	parts  int
}

func (w *rateTestWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	w.writes.Add(1)
	w.bytes, w.parts = mb.Len(), len(mb)
	buf.ReleaseMulti(mb)
	return nil
}

func (*rateTestWriter) Close() error { return nil }

func TestRateWriterCancelsWaitAndReleasesBuffer(t *testing.T) {
	for _, stop := range []string{"close", "interrupt", "context"} {
		t.Run(stop, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := rate.NewLimiter(1, 1)
			r.Allow() // Next byte would wait a full second without cancellation.
			inner := new(rateTestWriter)
			w := New().RateWriter(ctx, inner, r)
			b := buf.New()
			b.Write([]byte("x"))
			done := make(chan error, 1)
			go func() { done <- w.WriteMultiBuffer(buf.MultiBuffer{b}) }()
			deadline := time.Now().Add(time.Second)
			for r.Tokens() >= 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if r.Tokens() >= 0 {
				t.Fatal("wait reservation did not start")
			}
			switch stop {
			case "close":
				common.Close(w)
			case "interrupt":
				common.Interrupt(w)
			case "context":
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) || inner.writes.Load() != 0 || b.Len() != 0 {
					t.Fatalf("err=%v writes=%d retained bytes=%d", err, inner.writes.Load(), b.Len())
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("closed connection retained a rate wait")
			}
		})
	}
}

func TestRateWriterLargeWriteAndReservationError(t *testing.T) {
	inner := new(rateTestWriter)
	w := New().RateWriter(context.Background(), inner, rate.NewLimiter(1000, 1))
	defer common.Close(w)
	if err := w.WriteMultiBuffer(buf.MultiBuffer{buf.FromBytes([]byte("datagram"))}); err != nil {
		t.Fatal(err)
	}
	if inner.writes.Load() != 1 || inner.bytes != 8 || inner.parts != 1 {
		t.Fatal("large write was split, dropped, or duplicated")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	r := rate.NewLimiter(1, 1)
	r.Allow()
	w = New().RateWriter(ctx, inner, r)
	defer common.Close(w)
	b := buf.New()
	b.Write([]byte("x"))
	if err := w.WriteMultiBuffer(buf.MultiBuffer{b}); err == nil || b.Len() != 0 || inner.writes.Load() != 1 {
		t.Fatalf("reservation error ignored or payload retained: err=%v", err)
	}
}
