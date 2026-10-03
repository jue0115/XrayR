package splithttp

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestUploadPoolClose(t *testing.T) {
	pool := new(uploadConnPool)
	conn, peer := net.Pipe()
	defer peer.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	pool.Put(NewH1Conn(conn))
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if pool.Get() != nil {
		t.Fatal("closed pool retained an upload connection")
	}
	if err := pool.Close(); err != nil {
		t.Fatal("second close must be harmless:", err)
	}
	if _, err := conn.Write([]byte{1}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("pooled connection was not closed:", err)
	}

	conn, latePeer := net.Pipe()
	defer latePeer.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	pool.Put(NewH1Conn(conn))
	if _, err := conn.Write([]byte{1}); err == nil {
		t.Fatal("connection returned after pool close was not closed")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("connection remained open until its deadline")
	}
}
