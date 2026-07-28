package buf

import (
	"sync/atomic"
	"testing"
	"time"
)

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
