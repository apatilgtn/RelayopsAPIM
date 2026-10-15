package certs

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrRevoked is returned for a certificate listed in a loaded CRL.
var ErrRevoked = errors.New("certificate has been revoked")

// ParseCRLs reads certificate revocation lists from PEM ("X509 CRL" blocks)
// or a single DER-encoded list.
func ParseCRLs(data []byte) ([]*x509.RevocationList, error) {
	var out []*x509.RevocationList
	rest := data
	sawPEM := false
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		sawPEM = true
		if block.Type != "X509 CRL" {
			continue
		}
		l, err := x509.ParseRevocationList(block.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	if !sawPEM && len(data) > 0 {
		l, err := x509.ParseRevocationList(data)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	if len(out) == 0 {
		return nil, errors.New("no certificate revocation lists found")
	}
	return out, nil
}

type crl struct {
	list    *x509.RevocationList
	revoked map[string]bool // serial numbers, base 16
	// verified caches signature checks against issuer fingerprints, so a
	// handshake verifies each list's signature once per issuer.
	mu       sync.Mutex
	verified map[[32]byte]bool
}

// Revocations checks certificate chains against CRLs. A list is used only
// for certificates whose issuer signed it, so a list from one CA cannot
// revoke another CA's certificates.
type Revocations struct {
	byIssuer map[string][]*crl // issuer RawSubject
	all      []*crl
}

// NewRevocations indexes lists by issuer.
func NewRevocations(lists []*x509.RevocationList) *Revocations {
	r := &Revocations{byIssuer: map[string][]*crl{}}
	for _, l := range lists {
		c := &crl{list: l, revoked: map[string]bool{}, verified: map[[32]byte]bool{}}
		for _, e := range l.RevokedCertificateEntries {
			if e.SerialNumber != nil {
				c.revoked[e.SerialNumber.Text(16)] = true
			}
		}
		r.byIssuer[string(l.RawIssuer)] = append(r.byIssuer[string(l.RawIssuer)], c)
		r.all = append(r.all, c)
	}
	return r
}

// Len returns the number of lists.
func (r *Revocations) Len() int {
	if r == nil {
		return 0
	}
	return len(r.all)
}

// Stale counts lists past their NextUpdate. They are still enforced.
func (r *Revocations) Stale(now time.Time) int {
	if r == nil {
		return 0
	}
	n := 0
	for _, c := range r.all {
		if !c.list.NextUpdate.IsZero() && now.After(c.list.NextUpdate) {
			n++
		}
	}
	return n
}

func (c *crl) signedBy(issuer *x509.Certificate) bool {
	fp := sha256.Sum256(issuer.Raw)
	c.mu.Lock()
	ok, done := c.verified[fp]
	c.mu.Unlock()
	if done {
		return ok
	}
	ok = c.list.CheckSignatureFrom(issuer) == nil
	c.mu.Lock()
	c.verified[fp] = ok
	c.mu.Unlock()
	return ok
}

// IsRevoked reports whether cert, issued by issuer, is on a list issuer signed.
func (r *Revocations) IsRevoked(cert, issuer *x509.Certificate) bool {
	if r == nil || cert == nil || issuer == nil || cert.SerialNumber == nil {
		return false
	}
	serial := cert.SerialNumber.Text(16)
	for _, c := range r.byIssuer[string(issuer.RawSubject)] {
		if c.revoked[serial] && c.signedBy(issuer) {
			return true
		}
	}
	return false
}

// CheckChains refuses a connection when any verified chain contains a
// revoked certificate.
func (r *Revocations) CheckChains(chains [][]*x509.Certificate) error {
	if r.Len() == 0 {
		return nil
	}
	for _, chain := range chains {
		for i := 0; i+1 < len(chain); i++ {
			if r.IsRevoked(chain[i], chain[i+1]) {
				return fmt.Errorf("%w: serial %s issued by %s", ErrRevoked, chain[i].SerialNumber.Text(16), chain[i].Issuer.String())
			}
		}
	}
	return nil
}
