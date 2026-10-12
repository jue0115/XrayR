package buf

import (
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type interruptibleTimeoutReader struct {
	done chan struct{}
	once sync.Once
}

func (r *interruptibleTimeoutReader) ReadMultiBuffer() (MultiBuffer, error) {
	<-r.done
	return nil, io.EOF
}

func (r *interruptibleTimeoutReader) Interrupt() {
	r.once.Do(func() { close(r.done) })
}

func TestTimeoutWrapperReaderInterruptsPendingRead(t *testing.T) {
	for _, closeReader := range []bool{false, true} {
		r := &interruptibleTimeoutReader{done: make(chan struct{})}
		defer r.Interrupt()
		w := &TimeoutWrapperReader{Reader: r}
		if _, err := w.ReadMultiBufferTimeout(time.Millisecond); err != ErrReadTimeout {
			t.Fatalf("expected pending read, got %v", err)
		}
		if closeReader {
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
		} else {
			w.Interrupt()
		}
		done := make(chan error, 1)
		go func() { _, err := w.ReadMultiBuffer(); done <- err }()
		select {
		case err := <-done:
			if err != io.EOF {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("pending read survived wrapper interruption")
		}
	}
}

type testReadResult struct {
	buffer MultiBuffer
	err    error
}

type blockingTestReader struct {
	results chan testReadResult
}

func (r *blockingTestReader) ReadMultiBuffer() (MultiBuffer, error) {
	result := <-r.results
	return result.buffer, result.err
}

type nativeTimeoutTestReader struct {
	called atomic.Int32
}

func (r *nativeTimeoutTestReader) ReadMultiBuffer() (MultiBuffer, error) {
	return nil, nil
}

func (r *nativeTimeoutTestReader) ReadMultiBufferTimeout(time.Duration) (MultiBuffer, error) {
	r.called.Add(1)
	return MultiBuffer{FromBytes([]byte("native"))}, nil
}

func TestTimeoutWrapperReaderUsesNativeTimeout(t *testing.T) {
	reader := new(nativeTimeoutTestReader)
	wrapper := &TimeoutWrapperReader{Reader: reader}

	buffer, err := wrapper.ReadMultiBufferTimeout(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer ReleaseMulti(buffer)
	if got := buffer.Len(); got != int32(len("native")) {
		t.Fatalf("unexpected buffer length: %d", got)
	}
	if reader.called.Load() != 1 {
		t.Fatal("native timeout reader was not used")
	}
}

func TestTimeoutWrapperReaderReusesPendingRead(t *testing.T) {
	reader := &blockingTestReader{results: make(chan testReadResult, 1)}
	wrapper := &TimeoutWrapperReader{Reader: reader}

	buffer, err := wrapper.ReadMultiBufferTimeout(5 * time.Millisecond)
	if err != ErrReadTimeout || buffer != nil {
		t.Fatalf("expected timeout without data, got buffer=%v err=%v", buffer, err)
	}

	reader.results <- testReadResult{buffer: MultiBuffer{FromBytes([]byte("pending"))}}
	buffer, err = wrapper.ReadMultiBuffer()
	if err != nil {
		t.Fatal(err)
	}
	defer ReleaseMulti(buffer)
	if got := buffer.Len(); got != int32(len("pending")) {
		t.Fatalf("unexpected buffer length: %d", got)
	}
}
