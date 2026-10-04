package log

import (
	"bytes"
	"log"
	"path/filepath"
	"regexp"
	"testing"
)

func TestLogWritersUseSecondPrecision(t *testing.T) {
	fileCreator, err := CreateFileLogWriter(filepath.Join(t.TempDir(), "error.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		creator WriterCreator
	}{
		{"stdout", CreateStdoutLogWriter()},
		{"stderr", CreateStderrLogWriter()},
		{"file", fileCreator},
	} {
		t.Run(test.name, func(t *testing.T) {
			writer := test.creator()
			if writer == nil {
				t.Fatal("writer creation failed")
			}
			defer writer.Close()
			var logger *log.Logger
			switch w := writer.(type) {
			case *consoleLogWriter:
				logger = w.logger
			case *fileLogWriter:
				logger = w.logger
			default:
				t.Fatalf("unexpected writer type %T", writer)
			}
			var output bytes.Buffer
			logger.SetOutput(&output)
			if err := writer.Write("[Debug] proxy: example"); err != nil {
				t.Fatal(err)
			}
			if !regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} \[Debug\] proxy: example\r?\n$`).MatchString(output.String()) {
				t.Fatalf("unexpected timestamp or process prefix: %q", output.String())
			}
		})
	}
}
