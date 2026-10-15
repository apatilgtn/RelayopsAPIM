package certs

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/relayops/apim/internal/config"
)

// maxKeyPairFileBytes bounds a certificate or key file.
const maxKeyPairFileBytes = 1 << 20

// KeyPairFile is a certificate and key read from files and re-read when they
// change, for clients that present a certificate on new connections.
type KeyPairFile struct {
	certPath, keyPath string
	interval          time.Duration
	source            string

	mu       sync.Mutex
	cur      *tls.Certificate
	certRaw  []byte
	keyRaw   []byte
	checked  time.Time
	failures int
}

// LoadKeyPairFile reads a key pair that is re-read at most every interval.
func LoadKeyPairFile(certPath, keyPath, source string, interval time.Duration) (*KeyPairFile, error) {
	k := &KeyPairFile{certPath: certPath, keyPath: keyPath, interval: interval, source: source}
	if err := k.reload(time.Now()); err != nil {
		return nil, err
	}
	return k, nil
}

func readLimited(path string) ([]byte, error) {
	return config.ReadFileLimited(path, maxKeyPairFileBytes)
}

// reload re-reads the files; callers hold k.mu or own k exclusively.
func (k *KeyPairFile) reload(now time.Time) error {
	k.checked = now
	certRaw, err := readLimited(k.certPath)
	if err != nil {
		return err
	}
	keyRaw, err := readLimited(k.keyPath)
	if err != nil {
		return err
	}
	if k.cur != nil && bytes.Equal(certRaw, k.certRaw) && bytes.Equal(keyRaw, k.keyRaw) {
		return nil
	}
	pair, err := tls.X509KeyPair(certRaw, keyRaw)
	if err != nil {
		return fmt.Errorf("%s: %v", k.certPath, err)
	}
	pair.Leaf = Leaf(&pair)
	if k.cur != nil {
		slog.Info("client certificate reloaded", "source", k.source, "file", k.certPath)
	}
	k.cur, k.certRaw, k.keyRaw = &pair, certRaw, keyRaw
	return nil
}

// Certificate returns the current key pair, re-reading the files when the
// interval has passed. A failed re-read keeps the previous pair.
func (k *KeyPairFile) Certificate() *tls.Certificate {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	if k.interval > 0 && now.Sub(k.checked) >= k.interval {
		if err := k.reload(now); err != nil {
			k.failures++
			if k.failures == 1 || k.failures%60 == 0 {
				slog.Error("client certificate reload failed; keeping the current certificate", "source", k.source, "err", err)
			}
		} else {
			k.failures = 0
		}
	}
	return k.cur
}

// Reload re-reads the files now, as on SIGHUP. A failed re-read keeps the
// previous pair.
func (k *KeyPairFile) Reload() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.reload(time.Now())
}

// GetClientCertificate implements tls.Config.GetClientCertificate.
func (k *KeyPairFile) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return k.Certificate(), nil
}

// Statuses describes the current certificate.
func (k *KeyPairFile) Statuses() []Status {
	k.mu.Lock()
	defer k.mu.Unlock()
	if leaf := Leaf(k.cur); leaf != nil {
		return []Status{Summarize(leaf, k.source, "control-plane", RoleClient)}
	}
	return nil
}
