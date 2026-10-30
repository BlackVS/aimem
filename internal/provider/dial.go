package provider

// Where a provider call may connect (audit A-7). A registry provider's
// token goes to its base_url, which an admin bearer can set, so no call
// through a registry endpoint may use the hub's network position to reach
// loopback, private or link-local services, for example through a name
// re-pointed after the save. The AIMEM_OPENAI_* pair is the host
// operator's own setting, which the admin API cannot change, so it is not
// guarded. The one override is a host-wide environment setting.

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"
)

// AllowPrivateEnv lets every provider call reach loopback, private and
// link-local addresses when set to "1", for a model server on the host or
// the local network.
const AllowPrivateEnv = "AIMEM_PROVIDER_ALLOW_PRIVATE"

// AllowPrivate reports whether AllowPrivateEnv is set.
func AllowPrivate() bool { return os.Getenv(AllowPrivateEnv) == "1" }

// BlockedIP reports whether ip is an address a provider call refuses.
func BlockedIP(ip net.IP) bool {
	// All of 0.0.0.0/8, not only 0.0.0.0: Linux routes the block to
	// the local host.
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 0 {
		return true
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsMulticast()
}

// BlockedErr is the refusal for host.
func BlockedErr(host string) error {
	return fmt.Errorf("base_url host %s is a loopback, private or link-local address (set %s=1 in the hub's environment to allow it)", host, AllowPrivateEnv)
}

// transport serves every provider call. Its dialer checks the resolved
// address, not the name, so DNS cannot steer a call past the policy; the
// flag is read at each dial, which lets one transport (and its connection
// pool) serve every call.
var transport = func() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: 15 * time.Second, Control: refuseBlockedDial}).DialContext
	return tr
}()

func refuseBlockedDial(_, address string, _ syscall.RawConn) error {
	if AllowPrivate() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip == nil || BlockedIP(ip) {
		return BlockedErr(host)
	}
	return nil
}

// HTTPClient is the client for every call that carries a provider token.
// fromEnv is Endpoint.FromEnv: the host's own AIMEM_OPENAI_* pair dials
// unguarded, a registry endpoint through the guard.
func HTTPClient(timeout time.Duration, fromEnv bool) *http.Client {
	if fromEnv {
		return &http.Client{Timeout: timeout}
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}
