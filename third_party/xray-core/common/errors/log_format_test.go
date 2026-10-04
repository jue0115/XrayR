package errors_test

import (
	"context"
	"io"
	"strings"
	"testing"

	c "github.com/xtls/xray-core/common/ctx"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/common/log"
)

type formatLogCapture struct {
	message log.Message
}

func (h *formatLogCapture) Handle(message log.Message) {
	h.message = message
}

func TestLogOmitsSessionID(t *testing.T) {
	handler := &formatLogCapture{}
	log.RegisterHandler(handler)
	t.Cleanup(func() { log.RegisterHandler(log.NewLogger(log.CreateStdoutLogWriter())) })
	ctx := c.ContextWithID(context.Background(), c.ID(1362630611))
	for _, test := range []struct {
		level string
		write func(context.Context, ...interface{})
	}{
		{"Debug", errors.LogDebug},
		{"Info", errors.LogInfo},
		{"Warning", errors.LogWarning},
		{"Error", errors.LogError},
	} {
		t.Run(test.level, func(t *testing.T) {
			test.write(ctx, "taking detour [IPv6_out] for tcp:[2001:db8::1]:443")
			output := handler.message.String()
			if strings.Contains(output, "1362630611") || !strings.HasPrefix(output, "["+test.level+"] ") {
				t.Fatalf("unexpected session prefix or log level: %s", output)
			}
			if !strings.Contains(output, "taking detour [IPv6_out] for tcp:[2001:db8::1]:443") {
				t.Fatalf("message brackets were changed: %s", output)
			}
		})
	}
	errors.LogWarningInner(ctx, io.EOF, "connection ends")
	if output := handler.message.String(); strings.Contains(output, "1362630611") || !strings.HasSuffix(output, "connection ends > EOF") {
		t.Fatalf("unexpected inner error formatting: %s", output)
	}
	if c.IDFromContext(ctx) != c.ID(1362630611) {
		t.Fatal("logging changed the internal session ID")
	}
}
