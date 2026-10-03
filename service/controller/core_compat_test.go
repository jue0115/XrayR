package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	gotls "crypto/tls"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/XrayR-project/XrayR/api"
	"github.com/XrayR-project/XrayR/app/mydispatcher"
	"github.com/XrayR-project/XrayR/common/mylego"
	"github.com/xtls/xray-core/app/proxyman"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol/tls/cert"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/proxy/freedom"
	proxyhysteria "github.com/xtls/xray-core/proxy/hysteria"
	"github.com/xtls/xray-core/proxy/hysteria/account"
	"github.com/xtls/xray-core/proxy/vless"
	"github.com/xtls/xray-core/transport/internet"
	transporthysteria "github.com/xtls/xray-core/transport/internet/hysteria"
	xtls "github.com/xtls/xray-core/transport/internet/tls"
)

// The server may present a file certificate whose name differs from client SNI.
// Certificate verification is the client's responsibility when unknown SNI is allowed.
func TestFileCertificateWithMismatchedSNI(t *testing.T) {
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
	for _, node := range []api.NodeInfo{
		{NodeType: "Vless", TransportProtocol: "tcp", EnableTLS: true, VlessFlow: "xtls-rprx-vision", Port: 443},
		{NodeType: "Hysteria", TransportProtocol: "hysteria", EnableTLS: true, Port: 443},
	} {
		t.Run(node.NodeType, func(t *testing.T) {
			config := &Config{ListenIP: "127.0.0.1", CertConfig: &mylego.CertConfig{
				CertMode: "file", CertFile: certFile, KeyFile: keyFile,
			}}
			inbound, err := InboundBuilder(config, &node, "compat-test")
			if err != nil {
				t.Fatal(err)
			}
			receiver, err := inbound.ReceiverSettings.GetInstance()
			if err != nil {
				t.Fatal(err)
			}
			stream := receiver.(*proxyman.ReceiverConfig).StreamSettings
			security, err := stream.SecuritySettings[0].GetInstance()
			if err != nil {
				t.Fatal(err)
			}
			tlsConfig := security.(*xtls.Config)
			if tlsConfig.RejectUnknownSni {
				t.Fatal("omitted RejectUnknownSni must default to false")
			}
			if tlsConfig.Certificate[0].CertificatePath != certFile || tlsConfig.Certificate[0].KeyPath != keyFile {
				t.Fatal("file certificate paths were not preserved")
			}
			if node.NodeType == "Hysteria" && (len(tlsConfig.NextProtocol) != 1 || tlsConfig.NextProtocol[0] != "h3") {
				t.Fatal("Hysteria2 must advertise h3 ALPN")
			}
			serverConfig := tlsConfig.GetTLSConfig()
			t.Cleanup(func() { xtls.StopCertificateWatchers(tlsConfig) })
			serverConn, clientConn := net.Pipe()
			defer serverConn.Close()
			defer clientConn.Close()
			deadline := time.Now().Add(5 * time.Second)
			_ = serverConn.SetDeadline(deadline)
			_ = clientConn.SetDeadline(deadline)
			server := gotls.Server(serverConn, serverConfig)
			client := gotls.Client(clientConn, &gotls.Config{
				ServerName: "client.example", InsecureSkipVerify: true,
				NextProtos: serverConfig.NextProtos,
			})
			serverResult := make(chan error, 1)
			go func() { serverResult <- server.Handshake() }()
			if err := client.Handshake(); err != nil {
				t.Fatal("client TLS handshake:", err)
			}
			if err := <-serverResult; err != nil {
				t.Fatal("server TLS handshake:", err)
			}
		})
	}
}

func TestVisionAndHysteriaUserCompatibility(t *testing.T) {
	c := &Controller{Tag: "compat-test", nodeInfo: &api.NodeInfo{VlessFlow: "xtls-rprx-vision"}}
	users := []api.UserInfo{{UUID: "00000000-0000-4000-8000-000000000001", Email: "test@example.com", UID: 1}}
	vision, err := c.buildVlessUser(&users)[0].Account.GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	if account := vision.(*vless.Account); account.Flow != "xtls-rprx-vision" {
		t.Fatal("Vision flow changed:", account.Flow)
	} else if _, err := account.AsAccount(); err != nil {
		t.Fatal(err)
	}
	hysteria, err := c.buildHysteriaUser(&users)[0].Account.GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	if hysteria.(*account.Account).Auth != users[0].UUID {
		t.Fatal("Hysteria2 authentication changed")
	}
}

func TestHysteria2FileCertificateTransfer(t *testing.T) {
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
	portReservation, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := portReservation.LocalAddr().(*net.UDPAddr).Port
	_ = portReservation.Close()
	node := &api.NodeInfo{NodeType: "Hysteria", TransportProtocol: "hysteria", EnableTLS: true, Port: uint32(port)}
	const tag = "hysteria-compat"
	config := &Config{ListenIP: "127.0.0.1", CertConfig: &mylego.CertConfig{
		CertMode: "file", CertFile: certFile, KeyFile: keyFile,
	}}
	inbound, err := InboundBuilder(config, node, tag)
	if err != nil {
		t.Fatal(err)
	}
	users := []api.UserInfo{{UUID: "test-hysteria-auth", Email: "test@example.com", UID: 1}}
	controller := &Controller{Tag: tag, nodeInfo: node}
	proxy, err := inbound.ProxySettings.GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	proxy.(*proxyhysteria.ServerConfig).Users = controller.buildHysteriaUser(&users)
	inbound.ProxySettings = serial.ToTypedMessage(proxy)
	server, err := core.New(&core.Config{
		App: []*serial.TypedMessage{
			serial.ToTypedMessage(&mydispatcher.Config{}),
			serial.ToTypedMessage(&proxyman.InboundConfig{}),
			serial.ToTypedMessage(&proxyman.OutboundConfig{}),
		},
		Inbound: []*core.InboundHandlerConfig{inbound},
		Outbound: []*core.OutboundHandlerConfig{{ProxySettings: serial.ToTypedMessage(&freedom.Config{
			FinalRules: []*freedom.FinalRuleConfig{{Action: freedom.RuleAction_Allow}},
		})}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	dispatcher := server.GetFeature(routing.DispatcherType()).(*mydispatcher.DefaultDispatcher)
	if err := dispatcher.Limiter.AddInboundLimiter(tag, 0, &users, nil); err != nil {
		t.Fatal(err)
	}
	if err := server.Start(); err != nil {
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
		_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
		_, _ = io.Copy(conn, conn)
	}()
	hash := sha256.Sum256(certificate.Certificate)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := transporthysteria.Dial(ctx, xnet.TCPDestination(xnet.LocalHostIP, xnet.Port(port)), &internet.MemoryStreamConfig{
		ProtocolName: "hysteria", ProtocolSettings: &transporthysteria.Config{Auth: users[0].UUID},
		SecuritySettings: &xtls.Config{ServerName: "client.example", NextProtocol: []string{"h3"}, PinnedPeerCertSha256: [][]byte{hash[:]}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if err := proxyhysteria.WriteTCPRequest(conn, echo.Addr().String()); err != nil {
		t.Fatal(err)
	}
	if ok, message, err := proxyhysteria.ReadTCPResponse(conn); err != nil || !ok {
		t.Fatalf("Hysteria2 TCP response: ok=%v, message=%q, error=%v", ok, message, err)
	}
	payload := bytes.Repeat([]byte("Hysteria2-regression-"), 1024)
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, response); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(response, payload) {
		t.Fatal("Hysteria2 transfer corrupted the payload")
	}
}
