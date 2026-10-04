package cmd

import (
	"bytes"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

func TestPlainTextLogFormat(t *testing.T) {
	timestamp := time.Date(2026, 10, 4, 11, 28, 43, 518604000, time.FixedZone("UTC+8", 8*60*60))
	for _, test := range []struct {
		name, message, want string
		level               log.Level
		fields              log.Fields
	}{
		{
			name: "controller info", level: log.InfoLevel, message: "Added 1 new users",
			fields: log.Fields{"Type": "Vless", "ID": 255, "Host": "https://panel.example"},
			want:   "2026/10/04 11:28:43 [Info] Added 1 new users https://panel.example|255|Vless\n",
		},
		{
			name: "node warning with extra field", level: log.WarnLevel, message: "retry",
			fields: log.Fields{"Host": "https://panel.example", "ID": 256, "Type": "Hysteria2", "attempt": 2},
			want:   "2026/10/04 11:28:43 [Warning] retry https://panel.example|256|Hysteria2 attempt=2\n",
		},
		{
			name: "partial node fields", level: log.InfoLevel, message: "test",
			fields: log.Fields{"ID": 255},
			want:   "2026/10/04 11:28:43 [Info] test ID=255\n",
		},
		{
			name: "single line node fields", level: log.InfoLevel, message: "test",
			fields: log.Fields{"Host": "https://panel.example\nother", "ID": 255, "Type": "Vless\r\n"},
			want:   "2026/10/04 11:28:43 [Info] test https://panel.example\\nother|255|Vless\\r\\n\n",
		},
		{name: "debug", level: log.DebugLevel, message: "test", want: "2026/10/04 11:28:43 [Debug] test\n"},
		{name: "warning", level: log.WarnLevel, message: "retry", want: "2026/10/04 11:28:43 [Warning] retry\n"},
		{name: "error", level: log.ErrorLevel, message: "failed", want: "2026/10/04 11:28:43 [Error] failed\n"},
		{name: "single line", level: log.InfoLevel, message: "first\nsecond\r\n", want: "2026/10/04 11:28:43 [Info] first\\nsecond\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			entry := &log.Entry{Time: timestamp, Level: test.level, Message: test.message, Data: test.fields, Buffer: new(bytes.Buffer)}
			output, err := (&plainTextFormatter{}).Format(entry)
			if err != nil {
				t.Fatal(err)
			}
			if string(output) != test.want {
				t.Fatalf("unexpected format:\n got: %q\nwant: %q", output, test.want)
			}
		})
	}
	if _, ok := log.StandardLogger().Formatter.(*plainTextFormatter); !ok {
		t.Fatal("application's default logger does not use the plain-text formatter")
	}
}
