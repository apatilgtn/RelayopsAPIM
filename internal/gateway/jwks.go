package gateway

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

var (
	ErrJWKNotFound   = errors.New("jwk key not found for token kid")
	ErrJWKInvalid    = errors.New("invalid jwk key format")
	ErrTokenIssuer   = errors.New("jwt issuer does not match configured oidc_issuer")
	ErrTokenAudience = errors.New("jwt audience does not match configured oidc_audience")
)

type JWKSManager struct {
	client *http.Client
	mu     sync.RWMutex
	cache  map[string]*cachedJWKS
}

type cachedJWKS struct {
	fetchedAt time.Time
	keys      map[string]crypto.PublicKey // kid -> public key
}

func NewJWKSManager() *JWKSManager {
	return &JWKSManager{
		client: &http.Client{Timeout: 10 * time.Second},
		cache:  make(map[string]*cachedJWKS),
	}
}

type jwksResponse struct {
	Keys []rawJWK `json:"keys"`
}

type rawJWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (jm *JWKSManager) GetKey(jwksURL, kid string) (crypto.PublicKey, error) {
	jm.mu.RLock()
	c, found := jm.cache[jwksURL]
	jm.mu.RUnlock()

	if found && time.Since(c.fetchedAt) < 1*time.Hour {
		if k, ok := c.keys[kid]; ok {
			return k, nil
		}
	}

	// Fetch or refresh
	jm.mu.Lock()
	defer jm.mu.Unlock()

	// Double-check after lock
	c, found = jm.cache[jwksURL]
	if found && time.Since(c.fetchedAt) < 10*time.Second {
		if k, ok := c.keys[kid]; ok {
			return k, nil
		}
	}

	resp, err := jm.client.Get(jwksURL)
	if err != nil {
		return nil, fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch jwks: HTTP %d", resp.StatusCode)
	}

	var data jwksResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("decode jwks: %w", err)
	}

	newKeys := make(map[string]crypto.PublicKey)
	for _, raw := range data.Keys {
		pub, err := parseJWK(raw)
		if err == nil && pub != nil {
			newKeys[raw.Kid] = pub
		}
	}

	jm.cache[jwksURL] = &cachedJWKS{
		fetchedAt: time.Now(),
		keys:      newKeys,
	}

	if k, ok := newKeys[kid]; ok {
		return k, nil
	}
	if kid == "" && len(newKeys) == 1 {
		for _, k := range newKeys {
			return k, nil
		}
	}
	return nil, ErrJWKNotFound
}

func parseJWK(raw rawJWK) (crypto.PublicKey, error) {
	switch raw.Kty {
	case "RSA":
		nBytes, err := base64.RawURLEncoding.DecodeString(raw.N)
		if err != nil {
			return nil, err
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(raw.E)
		if err != nil {
			return nil, err
		}
		if len(eBytes) < 4 {
			pad := make([]byte, 4-len(eBytes))
			eBytes = append(pad, eBytes...)
		}
		eInt := int(binary.BigEndian.Uint32(eBytes))
		return &rsa.PublicKey{
			N: new(big.Int).SetBytes(nBytes),
			E: eInt,
		}, nil

	case "EC":
		xBytes, err := base64.RawURLEncoding.DecodeString(raw.X)
		if err != nil {
			return nil, err
		}
		yBytes, err := base64.RawURLEncoding.DecodeString(raw.Y)
		if err != nil {
			return nil, err
		}
		var curve elliptic.Curve
		switch raw.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, fmt.Errorf("unsupported curve %s", raw.Crv)
		}
		return &ecdsa.PublicKey{
			Curve: curve,
			X:     new(big.Int).SetBytes(xBytes),
			Y:     new(big.Int).SetBytes(yBytes),
		}, nil
	}
	return nil, fmt.Errorf("unsupported kty %s", raw.Kty)
}

// verifyOIDC validates an asymmetric JWT (RS256/ES256) using JWKS and checks claims.
func (jm *JWKSManager) VerifyOIDC(token, jwksURL, expectedIssuer, expectedAudience string, now time.Time) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errTokenMalformed
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, errTokenMalformed
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, errTokenMalformed
	}

	pubKey, err := jm.GetKey(jwksURL, header.Kid)
	if err != nil {
		return nil, fmt.Errorf("jwks key resolution: %w", err)
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errTokenMalformed
	}

	signedContent := []byte(parts[0] + "." + parts[1])

	switch header.Alg {
	case "RS256":
		rsaPub, ok := pubKey.(*rsa.PublicKey)
		if !ok {
			return nil, ErrJWKInvalid
		}
		h := sha256.Sum256(signedContent)
		if err := rsa.VerifyPKCS1v15(rsaPub, crypto.SHA256, h[:], sig); err != nil {
			return nil, errTokenSignature
		}
	case "RS384":
		rsaPub, ok := pubKey.(*rsa.PublicKey)
		if !ok {
			return nil, ErrJWKInvalid
		}
		h := sha512.Sum384(signedContent)
		if err := rsa.VerifyPKCS1v15(rsaPub, crypto.SHA384, h[:], sig); err != nil {
			return nil, errTokenSignature
		}
	case "RS512":
		rsaPub, ok := pubKey.(*rsa.PublicKey)
		if !ok {
			return nil, ErrJWKInvalid
		}
		h := sha512.Sum512(signedContent)
		if err := rsa.VerifyPKCS1v15(rsaPub, crypto.SHA512, h[:], sig); err != nil {
			return nil, errTokenSignature
		}
	case "ES256":
		ecPub, ok := pubKey.(*ecdsa.PublicKey)
		if !ok {
			return nil, ErrJWKInvalid
		}
		if len(sig) != 64 {
			return nil, errTokenSignature
		}
		r := new(big.Int).SetBytes(sig[:32])
		s := new(big.Int).SetBytes(sig[32:])
		h := sha256.Sum256(signedContent)
		if !ecdsa.Verify(ecPub, h[:], r, s) {
			return nil, errTokenSignature
		}
	default:
		return nil, fmt.Errorf("unsupported oidc alg %s", header.Alg)
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, errTokenMalformed
	}
	var claims map[string]any
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return nil, errTokenMalformed
	}

	const leeway = 60 * time.Second
	exp, hasExp := claims["exp"].(float64)
	if !hasExp {
		return nil, errors.New("token missing required exp claim")
	}
	if now.After(time.Unix(int64(exp), 0).Add(leeway)) {
		return nil, errTokenExpired
	}
	if nbf, ok := claims["nbf"].(float64); ok && now.Add(leeway).Before(time.Unix(int64(nbf), 0)) {
		return nil, errTokenNotYet
	}
	if expectedIssuer != "" {
		if iss, _ := claims["iss"].(string); iss != expectedIssuer {
			return nil, ErrTokenIssuer
		}
	}
	if expectedAudience != "" {
		switch aud := claims["aud"].(type) {
		case string:
			if aud != expectedAudience {
				return nil, ErrTokenAudience
			}
		case []any:
			matched := false
			for _, a := range aud {
				if s, ok := a.(string); ok && s == expectedAudience {
					matched = true
					break
				}
			}
			if !matched {
				return nil, ErrTokenAudience
			}
		default:
			return nil, ErrTokenAudience
		}
	}

	return claims, nil
}
