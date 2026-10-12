package tls

import (
	"runtime"
	"testing"
)

func TestRootCAPolicy(t *testing.T) {
	builtIn := (&Config{}).GetTLSConfig()
	if builtIn.RootCAs == nil || len(builtIn.RootCAs.Subjects()) == 0 {
		t.Fatal("default TLS config must use the built-in root CA bundle")
	}
	system := (&Config{UseSystemCa: true}).GetTLSConfig()
	if runtime.GOOS == "windows" && system.RootCAs != nil {
		t.Fatal("Windows system CA mode must use the native verifier")
	}
	if runtime.GOOS != "windows" && system.RootCAs == nil {
		t.Fatal("system CA mode must load system roots")
	}
	custom := (&Config{DisableSystemRoot: true}).GetTLSConfig()
	if custom.RootCAs == nil || len(custom.RootCAs.Subjects()) != 0 {
		t.Fatal("disableSystemRoot must still exclude default roots")
	}
}
