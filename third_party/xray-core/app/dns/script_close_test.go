package dns

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	featureDNS "github.com/xtls/xray-core/features/dns"
)

type closableScriptServer struct {
	scriptNameServer
	closed bool
}

func (s *closableScriptServer) Close() error { s.closed = true; return nil }

func TestDNSScriptCloseReleasesPoolAndServers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "close.lua")
	if err := os.WriteFile(path, []byte("function HandleDNSQuery() return nil end"), 0600); err != nil {
		t.Fatal(err)
	}
	upstream := &closableScriptServer{}
	server := &DNS{ctx: context.Background(), scriptPath: path, clients: []*Client{{server: upstream}}}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if !upstream.closed || server.clients != nil {
		t.Fatal("DNS close must release its upstream clients")
	}
	if _, _, err := server.script.query("example.com", featureDNS.IPOption{IPv4Enable: true}); err == nil {
		t.Fatal("DNS close must release the Lua state pool")
	}
	if err := server.Close(); err != nil {
		t.Fatal("second DNS close must be harmless:", err)
	}
}
