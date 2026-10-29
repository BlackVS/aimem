// Package childenv builds the environment for a child process that must
// not see the parent's secrets. A hub folds its env file (the admin
// token, provider keys) into its own environment, and a child that
// inherits it can be steered by the text it processes into reading it
// (audit A-22). Minimal passes what a program needs to run and nothing
// else.
package childenv

import (
	"os"
	"runtime"
	"slices"
	"strings"
)

// base is what any child needs to start, find its files, reach the
// network and print text: the search path, home and user, temp dirs,
// locale, proxies and CA bundles, and the Windows system set.
var base = []string{
	"PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "TZ", "LANG", "LANGUAGE",
	"XDG_CONFIG_HOME", "XDG_RUNTIME_DIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
	"http_proxy", "https_proxy", "no_proxy", "all_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
	"SYSTEMROOT", "WINDIR", "SYSTEMDRIVE", "COMSPEC", "PATHEXT",
	"USERPROFILE", "APPDATA", "LOCALAPPDATA", "PROGRAMDATA",
	"PROGRAMFILES", "PROGRAMFILES(X86)", "PROGRAMW6432",
	"TEMP", "TMP", "HOMEDRIVE", "HOMEPATH", "USERNAME",
}

// Minimal returns the parent's environment reduced to base, LC_*, and
// the extra names given; an extra name ending in "*" keeps every name
// with that prefix. A name under AIMEM_ never passes, even when named:
// those are the hub's own settings and secrets.
func Minimal(extra ...string) []string {
	keep := map[string]bool{}
	prefixes := []string{"LC_"}
	for _, n := range append(append([]string{}, base...), extra...) {
		if p, ok := strings.CutSuffix(n, "*"); ok {
			prefixes = append(prefixes, fold(p))
		} else {
			keep[fold(n)] = true
		}
	}
	var out []string
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "" || strings.HasPrefix(strings.ToUpper(name), "AIMEM_") {
			continue
		}
		f := fold(name)
		if keep[f] || slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(f, p) }) {
			out = append(out, kv)
		}
	}
	return out
}

// fold compares names as the platform does: case-insensitively on
// Windows.
func fold(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}
