package controller

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/XrayR-project/XrayR/api"
	"github.com/xtls/xray-core/app/proxyman"
	clog "github.com/xtls/xray-core/common/log"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf"
	"github.com/xtls/xray-core/transport/internet"
)

type freedomCompatibilityLogger struct{ t *testing.T }

func (l *freedomCompatibilityLogger) Handle(message clog.Message) {
	if strings.Contains(message.String(), `"freedom.domainStrategy"`) {
		l.t.Errorf("legacy freedom setting must not emit a deprecation warning: %s", message.String())
	}
}

func TestFreedomOutboundCompatibility(t *testing.T) {
	clog.RegisterHandler(&freedomCompatibilityLogger{t: t})
	t.Cleanup(func() { clog.RegisterHandler(clog.NewLogger(clog.CreateStdoutLogWriter())) })

	checkStrategy := func(t *testing.T, outbound *core.OutboundHandlerConfig, want internet.DomainStrategy) {
		t.Helper()
		settings, err := outbound.SenderSettings.GetInstance()
		if err != nil {
			t.Fatal(err)
		}
		sender := settings.(*proxyman.SenderConfig)
		got := sender.GetStreamSettings().GetSocketSettings().GetDomainStrategy()
		if got != want {
			t.Fatalf("outbound domain strategy = %v, want %v", got, want)
		}
	}
	buildCustom := func(t *testing.T, input string) *core.OutboundHandlerConfig {
		t.Helper()
		var config conf.OutboundDetourConfig
		if err := json.Unmarshal([]byte(input), &config); err != nil {
			t.Fatal(err)
		}
		outbound, err := config.Build()
		if err != nil {
			t.Fatal(err)
		}
		return outbound
	}

	for _, tc := range []struct {
		strategy string
		want     internet.DomainStrategy
	}{
		{"AsIs", internet.DomainStrategy_AS_IS},
		{"UseIP", internet.DomainStrategy_USE_IP},
		{"UseIPv4", internet.DomainStrategy_USE_IP4},
		{"UseIPv6", internet.DomainStrategy_USE_IP6},
		{"UseIPv4v6", internet.DomainStrategy_USE_IP46},
		{"UseIPv6v4", internet.DomainStrategy_USE_IP64},
		{"ForceIP", internet.DomainStrategy_FORCE_IP},
		{"ForceIPv4", internet.DomainStrategy_FORCE_IP4},
		{"ForceIPv6", internet.DomainStrategy_FORCE_IP6},
		{"ForceIPv4v6", internet.DomainStrategy_FORCE_IP46},
		{"ForceIPv6v4", internet.DomainStrategy_FORCE_IP64},
	} {
		t.Run(tc.strategy, func(t *testing.T) {
			outbound, err := OutboundBuilder(&Config{SendIP: "0.0.0.0", EnableDNS: true, DNSType: tc.strategy}, &api.NodeInfo{}, "compat-test")
			if err != nil {
				t.Fatal(err)
			}
			checkStrategy(t, outbound, tc.want)
			input := fmt.Sprintf(`{"protocol":"freedom","settings":{"domainStrategy":%q}}`, tc.strategy)
			checkStrategy(t, buildCustom(t, input), tc.want)
		})
	}

	t.Run("controller-defaults", func(t *testing.T) {
		for _, enableDNS := range []bool{false, true} {
			outbound, err := OutboundBuilder(&Config{SendIP: "0.0.0.0", EnableDNS: enableDNS}, &api.NodeInfo{}, "compat-test")
			if err != nil {
				t.Fatal(err)
			}
			want := internet.DomainStrategy_AS_IS
			if enableDNS {
				want = internet.DomainStrategy_USE_IP
			}
			checkStrategy(t, outbound, want)
		}
	})
	for _, tc := range []struct {
		name  string
		input string
		want  internet.DomainStrategy
	}{
		{"custom-default", `{"protocol":"freedom","settings":{}}`, internet.DomainStrategy_AS_IS},
		{"modern-sockopt", `{"protocol":"freedom","streamSettings":{"sockopt":{"domainStrategy":"UseIPv6"}}}`, internet.DomainStrategy_USE_IP6},
		{"legacy-precedence", `{"protocol":"freedom","settings":{"domainStrategy":"UseIPv6"},"streamSettings":{"sockopt":{"domainStrategy":"UseIPv4"}}}`, internet.DomainStrategy_USE_IP6},
		{"direct-alias", `{"protocol":"direct","settings":{"domainStrategy":"UseIPv6"}}`, internet.DomainStrategy_USE_IP6},
	} {
		t.Run(tc.name, func(t *testing.T) { checkStrategy(t, buildCustom(t, tc.input), tc.want) })
	}
	t.Run("invalid-strategy", func(t *testing.T) {
		settings := json.RawMessage(`{"domainStrategy":"invalid"}`)
		config := &conf.OutboundDetourConfig{Protocol: "freedom", Settings: &settings}
		if _, err := config.Build(); err == nil {
			t.Fatal("invalid legacy domain strategy must still be rejected")
		}
	})
}
