package internet_test

import (
	"context"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/transport/internet"
)

func TestDefaultListenerDisablesMPTCP(t *testing.T) {
	t.Setenv("GODEBUG", os.Getenv("GODEBUG")+",multipathtcp=1")
	lc := &net.ListenConfig{}
	if !lc.MultipathTCP() {
		t.Fatal("test must enable the Go listener default")
	}
	control, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if !listenerUsesMPTCP(t, control) {
		t.Skip("host cannot negotiate MPTCP; a plain TCP fallback cannot validate the security default")
	}
	for _, tc := range []struct {
		name    string
		sockopt *internet.SocketConfig
	}{{"nil", nil}, {"empty", &internet.SocketConfig{}}} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := new(internet.DefaultListener).Listen(context.Background(), &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}, tc.sockopt)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			if listenerUsesMPTCP(t, listener) {
				t.Fatal("listener unexpectedly negotiated MPTCP")
			}
		})
	}
}

func listenerUsesMPTCP(t *testing.T, listener net.Listener) bool {
	t.Helper()
	if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	dialer.SetMultipathTCP(true)
	conn, err := dialer.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	peer, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	enabled, err := peer.(*net.TCPConn).MultipathTCP()
	if err != nil {
		t.Fatal(err)
	}
	return enabled
}

func TestRegisterListenerController(t *testing.T) {
	var gotFd uintptr

	common.Must(internet.RegisterListenerController(func(network, address string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) {
			gotFd = fd
		})
	}))

	conn, err := internet.ListenSystemPacket(context.Background(), &net.UDPAddr{
		IP: net.IPv4zero,
	}, nil)
	common.Must(err)
	common.Must(conn.Close())

	if gotFd == 0 {
		t.Error("expected none-zero fd, but actually 0")
	}
}
