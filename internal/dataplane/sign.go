package dataplane

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/relayops/apim/internal/store"
)

// ConfigPayload is the signed content of a configuration response. It travels
// as raw JSON so gateways verify exactly the bytes the control plane signed.
type ConfigPayload struct {
	Data    store.SnapshotData    `json:"data"`
	Streams []store.StreamService `json:"streams"`
}

// ErrUnsigned is returned when signatures are required but a response has none.
var ErrUnsigned = errors.New("configuration is not signed")

// signingInput binds a signature to the payload (through its fingerprint)
// and to the moment it was issued.
func signingInput(fingerprint string, issuedAt time.Time) []byte {
	return []byte("relayops-dataplane-config-v1\n" + fingerprint + "\n" + issuedAt.UTC().Format(time.RFC3339Nano))
}

// Fingerprint is the hex SHA-256 of a payload.
func Fingerprint(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// KeyID identifies a public key in responses and logs.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// Signer signs configuration responses on the control plane.
type Signer struct {
	priv  ed25519.PrivateKey
	keyID string
}

// ParseSigningKey accepts a base64 Ed25519 seed (32 bytes) or private key (64 bytes).
func ParseSigningKey(b64 string) (*Signer, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("signing key is not base64: %w", err)
	}
	var priv ed25519.PrivateKey
	switch len(raw) {
	case ed25519.SeedSize:
		priv = ed25519.NewKeyFromSeed(raw)
	case ed25519.PrivateKeySize:
		priv = ed25519.PrivateKey(raw)
	default:
		return nil, fmt.Errorf("signing key must be a %d-byte seed or %d-byte private key, got %d bytes", ed25519.SeedSize, ed25519.PrivateKeySize, len(raw))
	}
	return &Signer{priv: priv, keyID: KeyID(priv.Public().(ed25519.PublicKey))}, nil
}

// PublicKey is the base64 public key gateways configure to verify signatures.
func (s *Signer) PublicKey() string {
	return base64.StdEncoding.EncodeToString(s.priv.Public().(ed25519.PublicKey))
}

func (s *Signer) KeyID() string { return s.keyID }

// Sign fills in the response's key ID and signature.
func (s *Signer) Sign(r *ConfigResponse) {
	r.KeyID = s.keyID
	r.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(s.priv, signingInput(r.Fingerprint, r.IssuedAt)))
}

// GenerateSigningKey returns a new base64 seed and its base64 public key.
func GenerateSigningKey() (seed, public string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(priv.Seed()), base64.StdEncoding.EncodeToString(pub), nil
}

// Verifier checks signatures on gateways. Several keys may be trusted at once
// so the control plane's key can be rotated without downtime.
type Verifier struct {
	keys map[string]ed25519.PublicKey
}

// ParseVerifyKeys parses a comma-separated list of base64 Ed25519 public keys.
func ParseVerifyKeys(csv string) (*Verifier, error) {
	v := &Verifier{keys: map[string]ed25519.PublicKey{}}
	for _, k := range strings.Split(csv, ",") {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(k)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("verify key %q is not a base64 %d-byte Ed25519 public key", k, ed25519.PublicKeySize)
		}
		pub := ed25519.PublicKey(raw)
		v.keys[KeyID(pub)] = pub
	}
	if len(v.keys) == 0 {
		return nil, errors.New("no verify keys given")
	}
	return v, nil
}

func (v *Verifier) verify(r *ConfigResponse) error {
	if r.Signature == "" {
		return ErrUnsigned
	}
	sig, err := base64.StdEncoding.DecodeString(r.Signature)
	if err != nil {
		return fmt.Errorf("signature is not base64: %w", err)
	}
	pub, ok := v.keys[r.KeyID]
	if !ok {
		return fmt.Errorf("configuration signed with untrusted key %q", r.KeyID)
	}
	if !ed25519.Verify(pub, signingInput(r.Fingerprint, r.IssuedAt), sig) {
		return errors.New("configuration signature is invalid")
	}
	return nil
}

// Open checks the payload against its fingerprint and, with a verifier, the
// signature, then decodes it. A nil verifier skips signature checks.
func (r *ConfigResponse) Open(v *Verifier) (*ConfigPayload, error) {
	if len(r.Payload) == 0 || r.Fingerprint == "" {
		return nil, errors.New("configuration response has no payload or fingerprint")
	}
	if Fingerprint(r.Payload) != r.Fingerprint {
		return nil, errors.New("configuration payload does not match its fingerprint")
	}
	if v != nil {
		if err := v.verify(r); err != nil {
			return nil, err
		}
	}
	var p ConfigPayload
	if err := json.Unmarshal(r.Payload, &p); err != nil {
		return nil, fmt.Errorf("decode configuration payload: %w", err)
	}
	return &p, nil
}
