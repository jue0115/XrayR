package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/XrayR-project/XrayR/api"
	"github.com/XrayR-project/XrayR/app/mydispatcher"
	"github.com/XrayR-project/XrayR/common/mylego"
	corepolicy "github.com/xtls/xray-core/app/policy"
	"github.com/xtls/xray-core/app/proxyman"
	corestats "github.com/xtls/xray-core/app/stats"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/protocol/tls/cert"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/features/stats"
	"github.com/xtls/xray-core/proxy/freedom"
	"github.com/xtls/xray-core/proxy/vless/inbound"
	"github.com/xtls/xray-core/proxy/vless/outbound"
	"github.com/xtls/xray-core/transport/internet"
	xtls "github.com/xtls/xray-core/transport/internet/tls"
)

// Exercise the real Vision direct-link path with XrayR's dispatcher, stats and
// limiter, not only the upstream dispatcher or synthetic interruptible readers.
func TestLifecycleVisionTransferAndCounters(t *testing.T) {
	certificate, err := cert.Generate(nil, cert.DNSNames("certificate.example"))
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := certificate.ToPEM()
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reservation.Addr().(*net.TCPAddr).Port
	reservation.Close()
	node := &api.NodeInfo{NodeType: "Vless", TransportProtocol: "tcp", EnableTLS: true, VlessFlow: "xtls-rprx-vision", Port: uint32(port)}
	const tag = "vision-lifecycle"
	c := &Controller{Tag: tag, nodeInfo: node}
	users := []api.UserInfo{lifecycleUser(1, "vision@example.invalid")}
	users[0].SpeedLimit = 1024 * 1024
	ib, err := InboundBuilder(&Config{ListenIP: "127.0.0.1", CertConfig: &mylego.CertConfig{CertMode: "file", CertFile: certFile, KeyFile: keyFile}}, node, tag)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := ib.ProxySettings.GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	proxy.(*inbound.Config).Users = c.buildVlessUser(&users)
	ib.ProxySettings = serial.ToTypedMessage(proxy)
	server, err := core.New(&core.Config{
		App: []*serial.TypedMessage{
			serial.ToTypedMessage(&mydispatcher.Config{}),
			serial.ToTypedMessage(&proxyman.InboundConfig{}),
			serial.ToTypedMessage(&proxyman.OutboundConfig{}),
			serial.ToTypedMessage(&corestats.Config{}),
			serial.ToTypedMessage(&corepolicy.Config{Level: map[uint32]*corepolicy.Policy{0: {Stats: &corepolicy.Policy_Stats{UserUplink: true, UserDownlink: true}}}}),
		},
		Inbound:  []*core.InboundHandlerConfig{ib},
		Outbound: []*core.OutboundHandlerConfig{{ProxySettings: serial.ToTypedMessage(&freedom.Config{FinalRules: []*freedom.FinalRuleConfig{{Action: freedom.RuleAction_Allow}}})}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	d := server.GetFeature(routing.DispatcherType()).(*mydispatcher.DefaultDispatcher)
	if err := d.Limiter.AddInboundLimiter(tag, 0, &users, nil); err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(certificate.Certificate)
	client, err := core.New(&core.Config{
		App: []*serial.TypedMessage{
			serial.ToTypedMessage(&mydispatcher.Config{}),
			serial.ToTypedMessage(&proxyman.InboundConfig{}),
			serial.ToTypedMessage(&proxyman.OutboundConfig{}),
		},
		Outbound: []*core.OutboundHandlerConfig{{
			ProxySettings: serial.ToTypedMessage(&outbound.Config{Vnext: &protocol.ServerEndpoint{
				Address: xnet.NewIPOrDomain(xnet.LocalHostIP), Port: uint32(port), User: c.buildVlessUser(&users)[0],
			}}),
			SenderSettings: serial.ToTypedMessage(&proxyman.SenderConfig{StreamSettings: &internet.StreamConfig{
				ProtocolName: "tcp", SecurityType: serial.GetMessageType(&xtls.Config{}),
				SecuritySettings: []*serial.TypedMessage{serial.ToTypedMessage(&xtls.Config{ServerName: "client.example", PinnedPeerCertSha256: [][]byte{hash[:]}})},
			}}),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		conn, err := echo.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(15 * time.Second))
		io.Copy(conn, conn)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := core.Dial(ctx, client, xnet.DestinationFromAddr(echo.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	payload := bytes.Repeat([]byte("Vision-lifecycle-"), 16384)
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, response); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, response) {
		t.Fatal("Vision payload corrupted")
	}
	c.stm = server.GetFeature(stats.ManagerType()).(stats.Manager)
	up, down, _, _ := c.getTraffic(c.buildUserTag(&users[0]))
	if up != int64(len(payload)) || down != int64(len(payload)) {
		t.Fatalf("Vision accounting changed: up=%d down=%d payload=%d", up, down, len(payload))
	}
}
