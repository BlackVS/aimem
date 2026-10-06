package enrollment

import (
	"crypto/hpke"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

var testPlain = Plaintext{Token: "aimem_user_sample-bearer-not-real", UserID: "u-1", TokenID: "t-1", HubID: "hub-1"}

func testAAD() []byte { return AAD("hub-1", "bundle-1", "k1_SAMPLE", "u-1", "t-1") }

// The suite is the contract's: DHKEM(X25519, HKDF-SHA256) (0x0020),
// HKDF-SHA256 (0x0001) and ChaCha20-Poly1305 (0x0003).
func TestSuiteIdentifiers(t *testing.T) {
	kdf, aead := suite()
	if kdf.ID() != 0x0001 || aead.ID() != 0x0003 {
		t.Fatalf("kdf %#04x aead %#04x", kdf.ID(), aead.ID())
	}
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	pk, err := hpke.NewDHKEMPublicKey(key.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if id := pk.KEM().ID(); id != 0x0020 {
		t.Fatalf("kem %#04x", id)
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ParsePublicKey(EncodePublicKey(key.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	enc, ct, err := Seal(Suite, pub, testAAD(), testPlain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ct, testPlain.Token) || strings.Contains(enc, testPlain.Token) {
		t.Fatal("the bearer is visible in the delivery")
	}
	got, err := Open(Suite, key, testAAD(), enc, ct)
	if err != nil || got != testPlain {
		t.Fatalf("open: %+v %v", got, err)
	}
}

// A delivery opens only with the client's key, in its own context, unaltered.
func TestOpenRefusesAnythingElse(t *testing.T) {
	key, _ := NewKey()
	other, _ := NewKey()
	enc, ct, err := Seal(Suite, key.PublicKey(), testAAD(), testPlain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(Suite, other, testAAD(), enc, ct); err == nil {
		t.Error("another private key opened the delivery")
	}
	for i, aad := range [][]byte{
		AAD("hub-2", "bundle-1", "k1_SAMPLE", "u-1", "t-1"),
		AAD("hub-1", "bundle-2", "k1_SAMPLE", "u-1", "t-1"),
		AAD("hub-1", "bundle-1", "k1_OTHER", "u-1", "t-1"),
		AAD("hub-1", "bundle-1", "k1_SAMPLE", "u-2", "t-1"),
		AAD("hub-1", "bundle-1", "k1_SAMPLE", "u-1", "t-2"),
	} {
		if _, err := Open(Suite, key, aad, enc, ct); err == nil {
			t.Errorf("aad variant %d opened the delivery", i)
		}
	}
	raw, _ := base64.RawURLEncoding.DecodeString(ct)
	raw[len(raw)/2] ^= 1
	if _, err := Open(Suite, key, testAAD(), enc, base64.RawURLEncoding.EncodeToString(raw)); err == nil {
		t.Error("a changed ciphertext opened")
	}
	if _, err := Open("hpke-other", key, testAAD(), enc, ct); err != ErrSuite {
		t.Errorf("unknown suite on open: %v", err)
	}
	if _, _, err := Seal("hpke-other", key.PublicKey(), testAAD(), testPlain); err != ErrSuite {
		t.Errorf("unknown suite on seal: %v", err)
	}
}

func TestParsePublicKey(t *testing.T) {
	for _, bad := range []string{"", "short", base64.RawURLEncoding.EncodeToString(make([]byte, 31)), base64.StdEncoding.EncodeToString(make([]byte, 32))} {
		if _, err := ParsePublicKey(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter(3, time.Minute)
	now := time.Now()
	for i := range 3 {
		if !l.Allow("a", now.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("event %d refused", i)
		}
	}
	if l.Allow("a", now.Add(3*time.Second)) {
		t.Fatal("the fourth event in the window was allowed")
	}
	if !l.Allow("b", now.Add(3*time.Second)) {
		t.Fatal("another key shares the limit")
	}
	// The first event leaves the window one minute after it.
	if !l.Allow("a", now.Add(time.Minute+time.Second/2)) {
		t.Fatal("the window did not slide")
	}
	if RedemptionsPerAddress != 10 || RedemptionsPerBundle != 20 || RateWindow != time.Minute {
		t.Fatal("the bounds differ from enrollment.v1")
	}
}

// The sweep forgets idle keys at most once per window: many distinct keys
// cost one sweep, not one scan per event.
func TestLimiterSweepsIdleKeysOncePerWindow(t *testing.T) {
	l := NewLimiter(1, time.Minute)
	now := time.Now()
	for i := range 1000 {
		l.Allow(fmt.Sprintf("address-%d", i), now)
	}
	if len(l.seen) != 1000 || !l.swept.Equal(now) {
		t.Fatalf("keys %d swept %v", len(l.seen), l.swept)
	}
	l.Allow("late", now.Add(30*time.Second)) // inside the window: no sweep
	if len(l.seen) != 1001 {
		t.Fatalf("a sweep ran inside the window: %d keys", len(l.seen))
	}
	l.Allow("later", now.Add(time.Minute+time.Second)) // a window later: the idle keys go
	if len(l.seen) != 2 {
		t.Fatalf("after the sweep: %d keys", len(l.seen))
	}
}

func BenchmarkLimiterDistinctKeys(b *testing.B) {
	l := NewLimiter(RedemptionsPerAddress, RateWindow)
	now := time.Now()
	for i := range b.N {
		l.Allow(fmt.Sprintf("address-%d", i), now)
	}
}
