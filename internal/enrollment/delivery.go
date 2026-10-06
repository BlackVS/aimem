// Package enrollment holds what the hub and the client share for enrollment
// delivery (docs/DESIGN-AIFORGE-ENROLLMENT-WIRE.md, "Delivery encryption"):
// the HPKE suite, the aad that binds a delivery to its context, and the
// sealing and opening of the plaintext that carries the bearer. The hub seals
// to the client's X25519 public key; only the client's private key opens it.
package enrollment

import (
	"crypto/ecdh"
	"crypto/hpke"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

const (
	// Suite is the delivery suite's wire name: HPKE base mode with
	// DHKEM(X25519, HKDF-SHA256), HKDF-SHA256 and ChaCha20-Poly1305
	// (KEM 0x0020, KDF 0x0001, AEAD 0x0003).
	Suite = "hpke-x25519-sha256-chacha20poly1305"
	// Info is the HPKE info string.
	Info = "aimem enrollment.v1 delivery"
)

// ErrSuite refuses any suite name but Suite.
var ErrSuite = errors.New("unknown delivery suite")

// Plaintext is what a delivery carries: the bearer and the identity it
// belongs to.
type Plaintext struct {
	Token   string `json:"token"`
	UserID  string `json:"user_id"`
	TokenID string `json:"token_id"`
	HubID   string `json:"hub_id"`
}

// AAD binds a delivery to its hub, bundle, request key (k1 form), user and
// token, so a ciphertext cannot be opened in any other context.
func AAD(hubID, bundleID, requestKey, userID, tokenID string) []byte {
	return []byte("enrollment.v1|" + hubID + "|" + bundleID + "|" + requestKey + "|" + userID + "|" + tokenID)
}

// NewKey generates the client's delivery key pair.
func NewKey() (*ecdh.PrivateKey, error) {
	return ecdh.X25519().GenerateKey(rand.Reader)
}

// EncodePublicKey is the wire form of a public key: unpadded base64url.
func EncodePublicKey(k *ecdh.PublicKey) string {
	return base64.RawURLEncoding.EncodeToString(k.Bytes())
}

// ParsePublicKey reads the wire form of an X25519 public key.
func ParsePublicKey(s string) (*ecdh.PublicKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("public_key must be 32 bytes of unpadded base64url")
	}
	return ecdh.X25519().NewPublicKey(raw)
}

func suite() (hpke.KDF, hpke.AEAD) { return hpke.HKDFSHA256(), hpke.ChaCha20Poly1305() }

// Seal encrypts p to the client's public key under aad. It returns the
// encapsulated key and the ciphertext, both in unpadded base64url.
func Seal(suiteName string, pub *ecdh.PublicKey, aad []byte, p Plaintext) (enc, ciphertext string, err error) {
	if suiteName != Suite {
		return "", "", ErrSuite
	}
	pk, err := hpke.NewDHKEMPublicKey(pub)
	if err != nil {
		return "", "", err
	}
	kdf, aead := suite()
	e, sender, err := hpke.NewSender(pk, kdf, aead, []byte(Info))
	if err != nil {
		return "", "", err
	}
	plain, err := json.Marshal(p)
	if err != nil {
		return "", "", err
	}
	ct, err := sender.Seal(aad, plain)
	if err != nil {
		return "", "", err
	}
	return base64.RawURLEncoding.EncodeToString(e), base64.RawURLEncoding.EncodeToString(ct), nil
}

// Open decrypts a delivery with the client's private key under aad. Any
// mismatch (another key, another context, a changed byte) fails.
func Open(suiteName string, priv *ecdh.PrivateKey, aad []byte, enc, ciphertext string) (Plaintext, error) {
	if suiteName != Suite {
		return Plaintext{}, ErrSuite
	}
	e, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return Plaintext{}, fmt.Errorf("enc is not base64url: %w", err)
	}
	ct, err := base64.RawURLEncoding.DecodeString(ciphertext)
	if err != nil {
		return Plaintext{}, fmt.Errorf("ciphertext is not base64url: %w", err)
	}
	sk, err := hpke.NewDHKEMPrivateKey(priv)
	if err != nil {
		return Plaintext{}, err
	}
	kdf, aead := suite()
	r, err := hpke.NewRecipient(e, sk, kdf, aead, []byte(Info))
	if err != nil {
		return Plaintext{}, err
	}
	plain, err := r.Open(aad, ct)
	if err != nil {
		return Plaintext{}, err
	}
	var p Plaintext
	if err := json.Unmarshal(plain, &p); err != nil {
		return Plaintext{}, fmt.Errorf("delivery plaintext: %w", err)
	}
	return p, nil
}
