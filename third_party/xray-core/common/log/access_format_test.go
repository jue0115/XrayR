package log_test

import (
	"testing"

	"github.com/xtls/xray-core/common/log"
)

func TestAccessLogEmailDisplay(t *testing.T) {
	for _, test := range []struct {
		name       string
		email      string
		detour     string
		want       string
		wantDetour string
	}{
		{"vless-default", "Vless_0.0.0.0_36361|742027193@qq.com|2", "Vless_0.0.0.0_36361 >> Vless_0.0.0.0_36361", "742027193@qq.com|2", "Vless_36361 >> Vless_36361"},
		{"vless-routed", "Vless_0.0.0.0_36361|742027193@qq.com|2", "Vless_0.0.0.0_36361 -> IPv6_out", "742027193@qq.com|2", "Vless_36361 -> IPv6_out"},
		{"hysteria-forced", "Hysteria2_::_443|user@example.com|3", "Hysteria2_::_443 ==> Socks1", "user@example.com|3", "Hysteria2_::_443 ==> Socks1"},
		{"hysteria-wildcard", "Hysteria2_0.0.0.0_443|user@example.com|3", "Hysteria2_0.0.0.0_443 ==> Socks1", "user@example.com|3", "Hysteria2_443 ==> Socks1"},
		{"different-ports", "Vless_0.0.0.0_36361|user@example.com|2", "Vless_0.0.0.0_36361 -> Vless_0.0.0.0_443", "user@example.com|2", "Vless_36361 -> Vless_443"},
		{"empty-email", "Vless_0.0.0.0_36361||2", "Vless_0.0.0.0_36361 >> direct", "|2", "Vless_36361 >> direct"},
		{"embedded-pipe", "Vless_0.0.0.0_36361|first|last@example.com|2", "Vless_0.0.0.0_36361 -> Socks1", "first|last@example.com|2", "Vless_36361 -> Socks1"},
		{"plain-email", "user@example.com", "custom >> direct", "user@example.com", "custom >> direct"},
		{"already-shortened", "user@example.com|2", "custom >> direct", "user@example.com|2", "custom >> direct"},
		{"custom-pipe", "first|last@example.com", "custom >> direct", "first|last@example.com", "custom >> direct"},
		{"no-uid", "custom|user@example.com", "custom >> direct", "custom|user@example.com", "custom >> direct"},
		{"different-inbound", "other|user@example.com|2", "custom >> direct", "other|user@example.com|2", "custom >> direct"},
		{"tag-prefix-collision", "custom|user@example.com|2", "custom2 >> direct", "custom|user@example.com|2", "custom2 >> direct"},
		{"other-listen-ip", "Vless_127.0.0.1_443|user@example.com|2", "Vless_127.0.0.1_443 >> Vless_127.0.0.1_443", "user@example.com|2", "Vless_127.0.0.1_443 >> Vless_127.0.0.1_443"},
		{"no-route-tag", "custom|user@example.com|2", "", "custom|user@example.com|2", ""},
		{"no-user", "", "Vless_0.0.0.0_36361 >> Vless_0.0.0.0_36361", "", "Vless_36361 >> Vless_36361"},
		{"outbound-only", "", "Vless_0.0.0.0_443", "", "Vless_443"},
	} {
		t.Run(test.name, func(t *testing.T) {
			message := &log.AccessMessage{
				From:   "127.0.0.1:12345",
				To:     "tcp:example.com:443",
				Status: log.AccessAccepted,
				Email:  test.email,
				Detour: test.detour,
			}
			want := "from 127.0.0.1:12345 accepted tcp:example.com:443"
			if test.detour != "" {
				want += " [" + test.wantDetour + "]"
			}
			if test.want != "" {
				want += " email:" + test.want
			}
			if got := message.String(); got != want {
				t.Fatalf("access log = %q, want %q", got, want)
			}
			if message.Email != test.email || message.Detour != test.detour {
				t.Fatal("formatting modified the internal user identity or routing tags")
			}
		})
	}
}
