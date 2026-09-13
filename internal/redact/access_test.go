package redact

import (
	"strings"
	"testing"
)

func TestOrdinaryAimemTokenIsScrubbedAndRefused(t *testing.T) {
	secret := "aimem_user_" + strings.Repeat("ab", 32)
	out, _ := String("issued "+secret+" once", 0)
	if strings.Contains(out, secret) || !strings.Contains(out, Placeholder) {
		t.Fatalf("token not scrubbed: %s", out)
	}
	_, refuse := ScanAuthored(secret)
	if len(refuse) != 1 || refuse[0] != "aimem user token" {
		t.Fatalf("token not refused: %v", refuse)
	}
}
