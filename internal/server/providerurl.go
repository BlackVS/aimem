package server

// Where a provider endpoint may point (audit A-7). A provider's stored
// token goes to its base_url, so the admin surface must not let a
// bearer aim that token at a host of its choosing, nor use the hub's
// network position to reach loopback, private or link-local services.
// The one override is a hub-wide environment setting: an admin bearer
// can edit the registry but not the service's environment.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"aimem/internal/provider"
)

const allowPrivateEnv = "AIMEM_PROVIDER_ALLOW_PRIVATE"

func providerAllowPrivate() bool { return os.Getenv(allowPrivateEnv) == "1" }

// lookupIP is the save-time resolver; tests replace it.
var lookupIP = net.DefaultResolver.LookupIPAddr

func blockedIP(ip net.IP) bool {
	// All of 0.0.0.0/8, not only 0.0.0.0: Linux routes the block to
	// the local host.
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 0 {
		return true
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsMulticast()
}

func blockedErr(host string) error {
	return fmt.Errorf("base_url host %s is a loopback, private or link-local address (set %s=1 in the hub's environment to allow it)", host, allowPrivateEnv)
}

// sameEndpoint compares base URLs the way the callers use them: blank
// means the default, and a trailing slash is trimmed before a path is
// appended.
func sameEndpoint(a, b string) bool {
	norm := func(u string) string {
		if u == "" {
			u = provider.DefaultBaseURL
		}
		return strings.TrimRight(u, "/")
	}
	return norm(a) == norm(b)
}

// checkProviderURL applies the save-time policy to a non-empty
// base_url. A name that does not resolve now is accepted: it may
// resolve later to anything, which is why the admin buttons also dial
// through providerClient.
func checkProviderURL(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("base_url is not a URL with a host")
	}
	if providerAllowPrivate() {
		return nil
	}
	if u.Scheme != "https" {
		return fmt.Errorf("base_url must be https (set %s=1 in the hub's environment for an http endpoint)", allowPrivateEnv)
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if blockedIP(ip) {
			return blockedErr(host)
		}
		return nil
	}
	if h := strings.ToLower(strings.TrimSuffix(host, ".")); h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return blockedErr(host)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	addrs, err := lookupIP(ctx, host)
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if blockedIP(a.IP) {
			return blockedErr(host + " (" + a.IP.String() + ")")
		}
	}
	return nil
}

// providerClient is the client for the admin-triggered calls that carry
// a stored token: the model list and the test probe. Unless the hub
// allows private endpoints, its dialer refuses blocked addresses after
// resolution, so a name re-pointed since the save gains nothing.
func providerClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if !providerAllowPrivate() {
		d := &net.Dialer{Timeout: 15 * time.Second, Control: refuseBlockedDial}
		tr.DialContext = d.DialContext
	}
	return &http.Client{Timeout: 15 * time.Second, Transport: tr}
}

func refuseBlockedDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip == nil || blockedIP(ip) {
		return blockedErr(host)
	}
	return nil
}
