package hysteria

import (
	"context"
	gotls "crypto/tls"
	"net/http"
	"net/url"
	"reflect"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/apernet/quic-go"
	"github.com/apernet/quic-go/http3"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/net/cnc"
	"github.com/xtls/xray-core/common/task"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/finalmask"
	"github.com/xtls/xray-core/transport/internet/hysteria/congestion"
	"github.com/xtls/xray-core/transport/internet/hysteria/congestion/bbr"
	"github.com/xtls/xray-core/transport/internet/stat"
	"github.com/xtls/xray-core/transport/internet/tls"
)

type client struct {
	lock         chan struct{}
	instance     *core.Instance
	forced       bool
	dest         net.Destination
	config       *Config
	tlsConfig    *gotls.Config
	tlsSource    *tls.Config
	socketConfig *internet.SocketConfig
	finalMask    *finalmask.FinalMask
	quicParams   *internet.QuicParams

	conn     *quic.Conn
	tr       *quic.Transport
	pktConn  net.PacketConn
	udpSM    *udpSessionManager
	lastUsed time.Time
}

func (c *client) status() status {
	if c.conn == nil {
		return StatusNull
	}
	select {
	case <-c.conn.Context().Done():
		return StatusInactive
	default:
		return StatusActive
	}
}

func (c *client) close() {
	if c.conn != nil {
		_ = c.conn.CloseWithError(closeErrCodeOK, "")
	}
	if c.tr != nil {
		_ = c.tr.Close()
	}
	if c.pktConn != nil {
		_ = c.pktConn.Close()
	}
	c.conn = nil
	c.tr = nil
	c.pktConn = nil
	c.udpSM = nil
}

func (c *client) dial(ctx context.Context) error {
	if c.forced {
		return errors.New("client is closed")
	}

	switch c.status() {
	case StatusActive:
		return nil
	case StatusInactive:
		c.close()
	}

	quicParams := c.quicParams
	if quicParams == nil {
		quicParams = &internet.QuicParams{
			BbrProfile: string(bbr.ProfileStandard),
		}
	}

	quicConfig := &quic.Config{
		InitialStreamReceiveWindow:     quicParams.InitStreamReceiveWindow,
		MaxStreamReceiveWindow:         quicParams.MaxStreamReceiveWindow,
		InitialConnectionReceiveWindow: quicParams.InitConnReceiveWindow,
		MaxConnectionReceiveWindow:     quicParams.MaxConnReceiveWindow,
		MaxIdleTimeout:                 time.Duration(quicParams.MaxIdleTimeout) * time.Second,
		KeepAlivePeriod:                time.Duration(quicParams.KeepAlivePeriod) * time.Second,
		DisablePathMTUDiscovery:        quicParams.DisablePathMtuDiscovery || (runtime.GOOS != "linux" && runtime.GOOS != "windows" && runtime.GOOS != "darwin"),
		ChromeParrot:                   !quicParams.DisableChromeParrot,
		EnableDatagrams:                true,
		MaxDatagramFrameSize:           MaxDatagramFrameSize,
		OmitMaxDatagramFrameSize:       true,
		DisablePathManager:             true,
	}
	if quicParams.InitStreamReceiveWindow == 0 {
		quicConfig.InitialStreamReceiveWindow = 8388608
	}
	if quicParams.MaxStreamReceiveWindow == 0 {
		quicConfig.MaxStreamReceiveWindow = 8388608
	}
	if quicParams.InitConnReceiveWindow == 0 {
		quicConfig.InitialConnectionReceiveWindow = 8388608 * 5 / 2
	}
	if quicParams.MaxConnReceiveWindow == 0 {
		quicConfig.MaxConnectionReceiveWindow = 8388608 * 5 / 2
	}
	if quicParams.MaxIdleTimeout == 0 {
		quicConfig.MaxIdleTimeout = 30 * time.Second
	}
	if quicParams.KeepAlivePeriod == 0 {
		quicConfig.KeepAlivePeriod = 10 * time.Second
	}

	var pktConn net.PacketConn
	var udpAddr net.Addr
	if c.finalMask != nil {
		conn, err := c.finalMask.DialUDP(ctx, c.dest)
		if err != nil {
			return errors.New("failed to dial to dest").Base(err)
		}
		pktConn = conn.(*net.PacketConnWrapper).PacketConn
		udpAddr = conn.RemoteAddr()
	} else {
		conn, err := internet.DialSystem(ctx, c.dest, c.socketConfig)
		if err != nil {
			return errors.New("failed to dial to dest").Base(err)
		}
		switch c := conn.(type) {
		case *net.PacketConnWrapper:
			pktConn = c.PacketConn
			udpAddr = c.RemoteAddr()
		case *cnc.Connection:
			pktConn = &internet.FakePacketConn{Conn: c}
			udpAddr = &net.UDPAddr{IP: []byte{0, 0, 0, 0}}
		default:
			panic(reflect.TypeOf(c))
		}
	}

	tr := &quic.Transport{Conn: pktConn, DisableGSO: quicParams.DisableGSO}

	if !quicParams.DisableChromeParrot {
		tr.ConnectionIDGenerator = quic.ZeroLengthConnectionIDGenerator{}
		c.tlsConfig.GetCertificate = nil
	}

	var conn *quic.Conn
	rt := &http3.Transport{
		TLSClientConfig: c.tlsConfig,
		QUICConfig:      quicConfig,
		Dial: func(ctx context.Context, _ string, tlsCfg *gotls.Config, cfg *quic.Config) (*quic.Conn, error) {
			qc, err := tr.DialEarly(ctx, udpAddr, tlsCfg, cfg)
			if err != nil {
				return nil, err
			}
			conn = qc
			return qc, nil
		},
	}
	req := &http.Request{
		Method: http.MethodPost,
		URL: &url.URL{
			Scheme: "https",
			Host:   URLHost,
			Path:   URLPath,
		},
		Header: http.Header{
			RequestHeaderAuth:   []string{c.config.Auth},
			CommonHeaderCCRX:    []string{strconv.FormatUint(quicParams.BrutalDown, 10)},
			CommonHeaderPadding: []string{AuthRequestPadding.String()},
		},
	}
	resp, err := rt.RoundTrip(req.WithContext(ctx))
	if err != nil {
		_ = rt.Close()
		if conn != nil {
			_ = conn.CloseWithError(closeErrCodeProtocolError, "")
		}
		_ = tr.Close()
		_ = pktConn.Close()
		return err
	}
	_ = resp.Body.Close()

	if resp.StatusCode != StatusAuthOK {
		_ = rt.Close()
		_ = conn.CloseWithError(closeErrCodeProtocolError, "")
		_ = tr.Close()
		_ = pktConn.Close()
		return errors.New("auth failed code ", resp.StatusCode)
	}

	// udp, _ := strconv.ParseBool(resp.Header.Get(ResponseHeaderUDPEnabled))
	down, _ := strconv.ParseUint(resp.Header.Get(CommonHeaderCCRX), 10, 64)
	errors.LogDebug(context.Background(), "ECHAccepted ", conn.ConnectionState().TLS.ECHAccepted)

	switch quicParams.Congestion {
	case "reno":
	case "bbr":
		congestion.UseBBR(conn, bbr.Profile(quicParams.BbrProfile))
	case "", "brutal":
		if quicParams.BrutalUp == 0 || down == 0 {
			congestion.UseBBR(conn, bbr.Profile(quicParams.BbrProfile))
		} else {
			congestion.UseBrutal(conn, min(quicParams.BrutalUp, down), quicParams.BrutalDisableLossCompensation)
		}
	case "force-brutal":
		congestion.UseBrutal(conn, quicParams.BrutalUp, quicParams.BrutalDisableLossCompensation)
	default:
		panic(quicParams.Congestion)
	}

	c.pktConn = pktConn
	c.tr = tr
	c.conn = conn
	c.udpSM = &udpSessionManager{
		conn: conn,
		m:    make(map[uint32]*InterConn),
		next: 1,
	}
	go c.udpSM.run()

	return nil
}

func (c *client) unlock() {
	<-c.lock
}

func (c *client) tcp(ctx context.Context) (stat.Connection, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case c.lock <- struct{}{}:
		defer c.unlock()
	}
	c.lastUsed = time.Now()

	err := c.dial(ctx)
	if err != nil {
		return nil, err
	}

	stream, err := c.conn.OpenStream()
	if err != nil {
		return nil, err
	}

	return &interConn{
		stream: stream,
		local:  c.conn.LocalAddr(),
		remote: c.conn.RemoteAddr(),

		client: true,
		lock:   make(chan struct{}, 1),
	}, nil
}

func (c *client) udp(ctx context.Context) (stat.Connection, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case c.lock <- struct{}{}:
		defer c.unlock()
	}
	c.lastUsed = time.Now()

	err := c.dial(ctx)
	if err != nil {
		return nil, err
	}

	return c.udpSM.udp()
}

func (c *client) clean(force bool) bool {
	c.lock <- struct{}{}
	defer c.unlock()
	if force {
		c.forced = true
	}
	status := c.status()
	remove := force || status == StatusInactive || (status == StatusNull && time.Since(c.lastUsed) > time.Minute)
	if remove {
		c.forced = true
		c.close()
		tls.StopCertificateWatchers(c.tlsSource)
	}
	return remove
}

type dialerConf struct {
	net.Destination
	*internet.MemoryStreamConfig
}

type clientManager struct {
	sync.RWMutex
	m           map[dialerConf]*client
	lifecycle   sync.RWMutex
	cleanupTask *task.Periodic
}

func (m *clientManager) clean() {
	m.RLock()
	clients := make(map[dialerConf]*client, len(m.m))
	for key, c := range m.m {
		clients[key] = c
	}
	m.RUnlock()
	// Never wait for instance lifecycle or an in-flight dial while holding the pool lock.
	for key, c := range clients {
		force := c.instance != nil && !c.instance.IsRunning()
		if c.clean(force) {
			m.Lock()
			if m.m[key] == c {
				delete(m.m, key)
			}
			m.Unlock()
		}
	}
}

func (m *clientManager) close() error {
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	m.Lock()
	cleanupTask := m.cleanupTask
	m.cleanupTask = nil
	clients := m.m
	m.m = make(map[dialerConf]*client)
	m.Unlock()
	if cleanupTask != nil {
		_ = cleanupTask.Close()
	}
	for _, c := range clients {
		c.clean(true)
	}
	return nil
}

var manager = &clientManager{m: make(map[dialerConf]*client)}

func Dial(ctx context.Context, dest net.Destination, streamSettings *internet.MemoryStreamConfig) (stat.Connection, error) {
	tlsConfig := tls.ConfigFromStreamSettings(streamSettings)
	if tlsConfig == nil {
		return nil, errors.New("tls config is nil")
	}

	datagram := DatagramFromContext(ctx)
	dest.Network = net.Network_UDP

	manager.lifecycle.RLock()
	defer manager.lifecycle.RUnlock()
	manager.Lock()
	startTask := false
	if manager.cleanupTask == nil {
		manager.cleanupTask = &task.Periodic{Interval: idleCleanupInterval, Execute: func() error {
			manager.clean()
			return nil
		}}
		startTask = true
	}
	cleanupTask := manager.cleanupTask
	c := manager.m[dialerConf{dest, streamSettings}]
	if c == nil {
		c = &client{
			lock:         make(chan struct{}, 1),
			lastUsed:     time.Now(),
			instance:     core.FromContext(ctx),
			dest:         dest,
			config:       streamSettings.ProtocolSettings.(*Config),
			tlsConfig:    tlsConfig.GetTLSConfig(tls.WithDestination(dest)),
			tlsSource:    tlsConfig,
			socketConfig: streamSettings.SocketSettings,
			finalMask:    streamSettings.FinalMask,
			quicParams:   streamSettings.QuicParams,
		}
		manager.m[dialerConf{dest, streamSettings}] = c
	}
	manager.Unlock()
	// Start executes the first cleanup synchronously, so it must run unlocked.
	if startTask {
		if err := cleanupTask.Start(); err != nil {
			manager.Lock()
			if manager.cleanupTask == cleanupTask {
				manager.cleanupTask = nil
			}
			manager.Unlock()
			return nil, err
		}
	}

	if datagram {
		return c.udp(ctx)
	}
	return c.tcp(ctx)
}

func init() {
	common.Must(internet.RegisterTransportDialer(protocolName, Dial))
	internet.RegisterTransportDialerCloser(manager.close)
}
