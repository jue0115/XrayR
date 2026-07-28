package tls

import (
	"testing"
	"time"
)

func TestStopCertificateWatchers(t *testing.T) {
	config := new(Config)
	called := make(chan struct{}, 1)
	config.setupOcspTicker(new(Certificate), func(bool, bool) {
		called <- struct{}{}
	})

	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("certificate watcher did not start")
	}

	StopCertificateWatchers(config)
	certificateWatchersAccess.Lock()
	_, found := certificateWatchers[config]
	certificateWatchersAccess.Unlock()
	if found {
		t.Fatal("certificate watcher was retained after stop")
	}

	// The stop operation is intentionally idempotent for repeated Core.Close.
	StopCertificateWatchers(config)
}
