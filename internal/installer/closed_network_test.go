package installer

// The installers read a release from an aiforge export (format 1) the same
// way they read it from github.com: <base>/MANIFEST names the release,
// <base>/aimem/<tag>/ holds its assets with SHA256SUMS, every file is
// checked before it is used, and with a base no other host is contacted.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

const exportTag = "v1.2.3"

// exportAssets is one release's assets: stub binaries that print which
// release they are, stub installers that report what boot found, and the
// plugin.
func exportAssets(tag string) map[string]string {
	a := map[string]string{
		"install.sh": `#!/bin/bash
d=$(cd "$(dirname "$0")" && pwd)
echo "installer ran: $1 tag=$AIMEM_TARGET_VERSION"
echo "binary: $("$AIMEM_PREBUILT")"
echo "plugin: $(cat "$d/.opencode/plugin/aimem.ts")"
`,
		"install.ps1": "param([string]$Target, [switch]$Bootstrap, [switch]$UserOnly)\r\n" +
			"$d = Split-Path -Parent $PSCommandPath\r\n" +
			"Write-Output \"installer ran: bootstrap=$Bootstrap tag=$env:AIMEM_TARGET_VERSION\"\r\n" +
			"Write-Output \"binary: $((Get-Content -Raw -LiteralPath $env:AIMEM_PREBUILT).Trim())\"\r\n" +
			"Write-Output \"plugin: $((Get-Content -Raw -LiteralPath (Join-Path $d '.opencode\\plugin\\aimem.ts')).Trim())\"\r\n",
		"aimem-opencode.ts":       "plugin-" + tag + "\n",
		"aimem-windows-amd64.exe": "bin-" + tag + "\n",
	}
	for _, p := range []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64"} {
		a["aimem-"+p] = "#!/bin/sh\necho bin-" + tag + "\n"
	}
	return a
}

// writeExport writes an export holding the given releases, each with a
// SHA256SUMS over its assets, and MANIFEST with the given text (none when
// manifest is empty).
func writeExport(t *testing.T, manifest string, tags ...string) string {
	t.Helper()
	base := filepath.Join(t.TempDir(), "export")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tag := range tags {
		dir := filepath.Join(base, "aimem", tag)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		var sums []string
		for name, body := range exportAssets(tag) {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			h := sha256.Sum256([]byte(body))
			sums = append(sums, hex.EncodeToString(h[:])+"  "+name)
		}
		sort.Strings(sums)
		if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(strings.Join(sums, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(base, "MANIFEST"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return base
}

// absRepo is a repository file's absolute path, for a run whose working
// directory is elsewhere.
func absRepo(t *testing.T, rel string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const goodManifest = "aiforge-export 1\naiskills v9.9.9\naimem " + exportTag + "\n"

// olderTag is a release published before its installer files became
// release assets: its SHA256SUMS lists only the binaries.
const olderTag = "v0.9.9"

// writeOlderRelease writes olderTag's assets into dir as such a release has
// them: the binaries and SHA256SUMS, no installer and no plugin.
func writeOlderRelease(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var sums []string
	for name, body := range exportAssets(olderTag) {
		if name == "install.sh" || name == "install.ps1" || name == "aimem-opencode.ts" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256([]byte(body))
		sums = append(sums, hex.EncodeToString(h[:])+"  "+name)
	}
	sort.Strings(sums)
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(strings.Join(sums, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// olderReleaseWeb is a web tree, served by mapping "https://" to it, that
// holds olderTag of the repository o/r: its release assets on github.com
// and its own boot script (name, body) in the repository at the tag.
func olderReleaseWeb(t *testing.T, name, body string) string {
	t.Helper()
	root := t.TempDir()
	writeOlderRelease(t, filepath.Join(root, "github.com", "o", "r", "releases", "download", olderTag))
	raw := filepath.Join(root, "raw.githubusercontent.com", "o", "r", olderTag)
	if err := os.MkdirAll(raw, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(raw, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// fakeCurl is a curl that logs every URL it is asked for and serves the
// ones under MAP_FROM from the directory MAP_TO; anything else fails.
const fakeCurl = `#!/bin/bash
url= out=
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift 2 ;;
    --proto | --proto-redir) shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
echo "$url" >> "$CURL_LOG"
case "$url" in
  "$MAP_FROM"*) cp "$MAP_TO/${url#"$MAP_FROM"}" "$out" 2>/dev/null || exit 22 ;;
  *) exit 6 ;;
esac
`

// runBoot runs boot.sh with sh in a fresh project directory, a fake curl
// first on PATH, and env added. It returns the output and the URLs curl
// was asked for.
func runBoot(t *testing.T, shell string, env ...string) (string, []string, error) {
	t.Helper()
	shellOnly(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	project := filepath.Join(dir, "project")
	for _, d := range []string{bin, project} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(fakeCurl), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "curl.log")
	cmd := exec.Command(shell, absRepo(t, "boot.sh"))
	cmd.Dir = project
	cmd.Env = append(withoutAimemEnv(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"CURL_LOG="+log, "MAP_FROM=https://nowhere.invalid/", "MAP_TO="+dir)
	cmd.Env = append(cmd.Env, env...)
	b, err := cmd.CombinedOutput()
	var urls []string
	if l, rerr := os.ReadFile(log); rerr == nil {
		urls = strings.Fields(string(l))
	}
	return string(b), urls, err
}

func TestBootShInstallsFromAnExportDirectory(t *testing.T) {
	base := writeExport(t, goodManifest, exportTag)
	for _, shell := range []string{"bash", "sh"} {
		out, urls, err := runBoot(t, shell, "AIMEM_BASE="+base)
		if err != nil {
			t.Fatalf("%s: %v\n%s", shell, err, out)
		}
		for _, want := range []string{"Fetching aimem " + exportTag + " from " + base + "/aimem/" + exportTag,
			"checksum OK: install.sh", "checksum OK: aimem-opencode.ts",
			"installer ran: bootstrap tag=" + exportTag, "binary: bin-" + exportTag, "plugin: plugin-" + exportTag} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: no %q in:\n%s", shell, want, out)
			}
		}
		if len(urls) != 0 || strings.Contains(out, "github.com") {
			t.Errorf("%s: an export install contacted %v or printed github.com:\n%s", shell, urls, out)
		}
	}
}

// Several releases sit side by side; MANIFEST names the default, an
// explicit AIMEM_VERSION wins and needs no MANIFEST. A MANIFEST written on
// Windows (CRLF) reads the same.
func TestBootShPicksTheRelease(t *testing.T) {
	crlf := strings.ReplaceAll(goodManifest, "\n", "\r\n")
	for _, c := range []struct {
		name, manifest, version, want string
	}{
		{"manifest", goodManifest, "", exportTag},
		{"crlf manifest", crlf, "", exportTag},
		{"version wins", goodManifest, "v2.0.0", "v2.0.0"},
		{"version without manifest", "", "v2.0.0", "v2.0.0"},
	} {
		base := writeExport(t, c.manifest, exportTag, "v2.0.0")
		out, _, err := runBoot(t, "bash", "AIMEM_BASE="+base+"/", "AIMEM_VERSION="+c.version)
		if err != nil || !strings.Contains(out, "binary: bin-"+c.want) || !strings.Contains(out, "tag="+c.want) {
			t.Errorf("%s: %v\n%s", c.name, err, out)
		}
	}
}

// Any problem with the export stops the install with a message naming the
// path; the installer never runs.
func TestBootShRefusesABadExport(t *testing.T) {
	good := func(t *testing.T) string { return writeExport(t, goodManifest, exportTag) }
	rel := func(base string) string { return filepath.Join(base, "aimem", exportTag) }
	for _, c := range []struct {
		name  string
		setup func(t *testing.T) (base string, env []string)
		want  string
	}{
		{"missing manifest", func(t *testing.T) (string, []string) {
			b := writeExport(t, "", exportTag)
			return b, nil
		}, "/MANIFEST is missing (or set AIMEM_VERSION"},
		{"unknown format", func(t *testing.T) (string, []string) {
			return writeExport(t, "aiforge-export 2\naimem "+exportTag+"\n", exportTag), nil
		}, "/MANIFEST is 'aiforge-export 2', not a format this installer reads"},
		{"no aimem line", func(t *testing.T) (string, []string) {
			return writeExport(t, "aiforge-export 1\naiskills v9.9.9\n", exportTag), nil
		}, "/MANIFEST names no aimem release"},
		{"manifest names a missing release", func(t *testing.T) (string, []string) {
			return writeExport(t, "aiforge-export 1\naimem v7.7.7\n"), nil
		}, "/aimem/v7.7.7/SHA256SUMS is missing"},
		{"missing asset", func(t *testing.T) (string, []string) {
			b := good(t)
			if err := os.Remove(filepath.Join(rel(b), "install.sh")); err != nil {
				t.Fatal(err)
			}
			return b, nil
		}, "/aimem/" + exportTag + "/install.sh is missing"},
		{"sum mismatch", func(t *testing.T) (string, []string) {
			b := good(t)
			if err := os.WriteFile(filepath.Join(rel(b), "aimem-opencode.ts"), []byte("tampered\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return b, nil
		}, "checksum mismatch for "},
		{"plain http", func(t *testing.T) (string, []string) {
			return "http://export.test", nil
		}, "AIMEM_BASE must be an absolute directory or an https:// URL, not 'http://export.test'"},
		{"relative base", func(t *testing.T) (string, []string) {
			return "export", nil
		}, "AIMEM_BASE must be an absolute directory"},
		{"tag with a path", func(t *testing.T) (string, []string) {
			return good(t), []string{"AIMEM_VERSION=v1/../../etc"}
		}, "'v1/../../etc' is not a release tag"},
	} {
		t.Run(c.name, func(t *testing.T) {
			base, env := c.setup(t)
			out, urls, err := runBoot(t, "bash", append([]string{"AIMEM_BASE=" + base}, env...)...)
			if err == nil || !strings.Contains(out, c.want) || !strings.Contains(out, "nothing was installed") {
				t.Fatalf("err %v, want %q in:\n%s", err, c.want, out)
			}
			if strings.Contains(out, "installer ran") || len(urls) != 0 {
				t.Fatalf("the installer ran or a host was contacted (%v):\n%s", urls, out)
			}
		})
	}
}

// An https export is read with the same rules, from that host only; the
// connected form reads the release download URL on github.com.
func TestBootShReadsHttpsExportsAndGitHub(t *testing.T) {
	base := writeExport(t, goodManifest, exportTag)
	out, urls, err := runBoot(t, "bash", "AIMEM_BASE=https://export.test/x", "MAP_FROM=https://export.test/x/", "MAP_TO="+base)
	if err != nil || !strings.Contains(out, "installer ran: bootstrap tag="+exportTag) || strings.Contains(out, "github.com") {
		t.Fatalf("https export: %v\n%s", err, out)
	}
	for _, u := range urls {
		if !strings.HasPrefix(u, "https://export.test/x/") {
			t.Errorf("https export contacted %s", u)
		}
	}
	if len(urls) == 0 || urls[0] != "https://export.test/x/MANIFEST" {
		t.Errorf("https export did not read MANIFEST first: %v", urls)
	}

	gh := "https://github.com/o/r/releases/download/"
	out, urls, err = runBoot(t, "bash", "AIMEM_REPO=o/r", "AIMEM_VERSION="+exportTag, "MAP_FROM="+gh, "MAP_TO="+filepath.Join(base, "aimem"))
	if err != nil || !strings.Contains(out, "installer ran: bootstrap tag="+exportTag) {
		t.Fatalf("connected: %v\n%s", err, out)
	}
	for _, u := range urls {
		if !strings.HasPrefix(u, gh+exportTag+"/") {
			t.Errorf("connected install fetched %s", u)
		}
	}
}

// A release published before its installer files became release assets is
// installed by its own boot.sh, fetched from the repository at its tag and
// run with the same environment; its exit status is boot.sh's. aicrew's
// instructions pipe master's boot.sh with AIMEM_VERSION naming such a
// release. With AIMEM_BASE there is no hand-over: the export is refused.
func TestBootShHandsOverToAnOlderRelease(t *testing.T) {
	sums := "https://github.com/o/r/releases/download/" + olderTag + "/SHA256SUMS"
	script := "https://raw.githubusercontent.com/o/r/" + olderTag + "/boot.sh"
	for _, c := range []struct {
		name, exit string
		fails      bool
	}{{"succeeds", "0", false}, {"fails", "7", true}} {
		t.Run(c.name, func(t *testing.T) {
			web := olderReleaseWeb(t, "boot.sh", "echo \"old boot ran: version=$AIMEM_VERSION repo=$AIMEM_REPO dir=$(basename \"$PWD\")\"\nexit "+c.exit+"\n")
			out, urls, err := runBoot(t, "bash", "AIMEM_REPO=o/r", "AIMEM_VERSION="+olderTag, "MAP_FROM=https://", "MAP_TO="+web)
			if (err != nil) != c.fails {
				t.Fatalf("err %v:\n%s", err, out)
			}
			for _, want := range []string{"aimem " + olderTag + " has no installer assets; handing over to that release's own boot script (" + script + ")",
				"old boot ran: version=" + olderTag + " repo=o/r dir=project"} {
				if !strings.Contains(out, want) {
					t.Errorf("no %q in:\n%s", want, out)
				}
			}
			if strings.Contains(out, "installer ran") || strings.Contains(out, "is missing") {
				t.Errorf("the new installer path ran:\n%s", out)
			}
			if strings.Join(urls, " ") != sums+" "+script {
				t.Errorf("fetched %v, want the sums then the old boot script", urls)
			}
		})
	}

	t.Run("export", func(t *testing.T) {
		base := filepath.Join(t.TempDir(), "export")
		writeOlderRelease(t, filepath.Join(base, "aimem", olderTag))
		out, urls, err := runBoot(t, "bash", "AIMEM_BASE="+base, "AIMEM_VERSION="+olderTag)
		if err == nil || !strings.Contains(out, "/aimem/"+olderTag+"/install.sh is missing") ||
			strings.Contains(out, "handing over") || len(urls) != 0 {
			t.Fatalf("err %v, urls %v:\n%s", err, urls, out)
		}
	})
}

// install-hub.sh runs alone (curl | bash), so it carries its own copy of
// boot.sh's release-source block; the copies must not drift.
func TestReleaseSourceBlocksAgree(t *testing.T) {
	if a, b := extract(t, "boot.sh", "release-source"), extract(t, "install-hub.sh", "release-source"); a != b {
		t.Fatal("boot.sh and install-hub.sh carry different release-source blocks; keep them identical")
	}
}

// The hub installer stages its binary from an export, verified, and
// refuses a base together with its own binary knobs.
func TestHubStagesFromAnExport(t *testing.T) {
	base := writeExport(t, goodManifest, exportTag)
	staged, out, err := runStage(t, "AIMEM_BASE="+base)
	if err != nil || !strings.Contains(out, "installing aimem "+exportTag+" (aimem-linux-") ||
		!strings.Contains(out, "from "+base+"/aimem/"+exportTag) || !strings.Contains(out, "checksum OK") {
		t.Fatalf("export: %v\n%s", err, out)
	}
	if b, err := os.ReadFile(staged); err != nil || !strings.Contains(string(b), "bin-"+exportTag) {
		t.Fatalf("staged %q (%v)", b, err)
	}
	if _, err := os.Stat(staged + ".src"); err == nil {
		t.Fatal("the download directory was left beside the binary")
	}
	for _, knob := range []string{"AIMEM_PREBUILT_DIR=" + base, "AIMEM_PREBUILT=" + staged} {
		if _, out, err := runStage(t, "AIMEM_BASE="+base, knob); err == nil || !strings.Contains(out, "are both set") {
			t.Errorf("%s with AIMEM_BASE: %v\n%s", knob, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "aimem", exportTag, "aimem-linux-amd64"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "aimem", exportTag, "aimem-linux-arm64"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	staged, out, err = runStage(t, "AIMEM_BASE="+base)
	if err == nil || !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("tampered export: %v\n%s", err, out)
	}
	if _, err := os.Stat(staged); err == nil {
		t.Fatal("a tampered binary was staged")
	}
}

// With a base, the client hint tells the next person to install from the
// same export, never from github.com, and each printed command survives a
// paste: the test runs the printed line itself, with a base that holds a
// space and a single quote.
func TestClientHintNamesTheExport(t *testing.T) {
	shellOnly(t)
	t.Setenv("TAG", exportTag)
	// hintLine is the command printed on the line after the heading.
	hintLine := func(t *testing.T, out, heading string) string {
		t.Helper()
		lines := strings.Split(out, "\n")
		for i, l := range lines {
			if strings.TrimSpace(l) == heading && i+1 < len(lines) {
				return strings.TrimSpace(lines[i+1])
			}
		}
		t.Fatalf("no %q in:\n%s", heading, out)
		return ""
	}
	runLine := func(t *testing.T, line string, env ...string) string {
		t.Helper()
		cmd := exec.Command("bash", "-c", line)
		cmd.Env = append(withoutAimemEnv(), env...)
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("the printed command failed: %v\n%s\n%s", err, line, b)
		}
		return string(b)
	}

	t.Run("directory", func(t *testing.T) {
		base := filepath.ToSlash(filepath.Join(t.TempDir(), "team export", "it's"))
		dir := filepath.Join(base, "aimem", exportTag)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "boot.sh"), []byte("echo \"ran base=[$AIMEM_BASE] script=[$0]\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AIMEM_BASE", base+"/")
		out, errs := runClientHint(t, "", "https")
		if strings.Contains(out+errs, "github") {
			t.Fatalf("the hint names github:\n%s%s", out, errs)
		}
		got := runLine(t, hintLine(t, out, "Linux/macOS, with the export mounted at the same path:"))
		if want := "ran base=[" + base + "] script=[" + base + "/aimem/" + exportTag + "/boot.sh]"; !strings.Contains(got, want) {
			t.Fatalf("want %q, got:\n%s", want, got)
		}
	})

	t.Run("https", func(t *testing.T) {
		base := "https://export.test/team x/it's"
		bin := t.TempDir()
		urlFile := filepath.Join(bin, "url")
		// The fake curl records the URL it was given and serves a script
		// that reports the base the pipe's bash received.
		curl := "#!/bin/bash\nfor a; do u=$a; done\nprintf '%s' \"$u\" > \"$URL_FILE\"\necho 'echo \"ran base=[$AIMEM_BASE]\"'\n"
		if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(curl), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AIMEM_BASE", base)
		out, errs := runClientHint(t, "", "https")
		if strings.Contains(out+errs, "github") {
			t.Fatalf("the hint names github:\n%s%s", out, errs)
		}
		got := runLine(t, hintLine(t, out, "Linux/macOS:"), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "URL_FILE="+urlFile)
		if want := "ran base=[" + base + "]"; !strings.Contains(got, want) {
			t.Fatalf("want %q, got:\n%s", want, got)
		}
		if u, _ := os.ReadFile(urlFile); string(u) != base+"/aimem/"+exportTag+"/boot.sh" {
			t.Fatalf("curl was given %q", u)
		}
		ps := hintLine(t, out, "Windows (PowerShell):")
		if want := "$env:AIMEM_BASE = 'https://export.test/team x/it''s'; try { irm 'https://export.test/team x/it''s/aimem/" + exportTag + "/boot.ps1' | iex } finally { Remove-Item Env:AIMEM_BASE }"; ps != want {
			t.Fatalf("PowerShell line:\n%s\nwant:\n%s", ps, want)
		}
	})
}

// boot.ps1 reads an export with the same rules: the release MANIFEST
// names, each file checked against SHA256SUMS on its whole name, the
// installer unblocked after its check (a copy from a downloaded export
// carries the Mark of the Web), nothing installed on any problem, and with
// a base no other host contacted. Invoke-WebRequest is a stub that logs
// each URL and serves those under MAP_FROM from MAP_TO.
//
// All cases run in one Windows PowerShell process, one after another, each
// with its own variables and fetch log, and each is checked in a subtest
// of its own. A Windows PowerShell start costs about 18 s on the CI runner
// (#357 ran one per case, 307 s in all, and pushed the package past the
// test timeout).
func TestBootPs1InstallsFromAnExport(t *testing.T) {
	ps := powerShell(t)
	if runtime.GOOS != "windows" {
		t.Skip("boot.ps1 runs on Windows")
	}
	dir := t.TempDir()
	project := filepath.Join(dir, "project")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	type result struct {
		out  string
		urls []string
	}
	type psCase struct {
		name  string
		env   map[string]string // AIMEM_BASE, AIMEM_VERSION, AIMEM_REPO, MAP_FROM, MAP_TO
		check func(t *testing.T, r result)
	}
	installed := func(r result, tag string) bool {
		return strings.Contains(r.out, "installer ran: bootstrap=True tag="+tag) &&
			strings.Contains(r.out, "binary: bin-"+tag) && strings.Contains(r.out, "plugin: plugin-"+tag) &&
			!strings.Contains(r.out, "failure=")
	}
	var cases []psCase

	marked := writeExport(t, strings.ReplaceAll(goodManifest, "\n", "\r\n"), exportTag)
	inst := filepath.Join(marked, "aimem", exportTag, "install.ps1")
	if err := os.WriteFile(inst+":Zone.Identifier", []byte("[ZoneTransfer]\r\nZoneId=3\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases = append(cases, psCase{"directory export, the installer marked as downloaded", map[string]string{"AIMEM_BASE": marked},
		func(t *testing.T, r result) {
			if !installed(r, exportTag) || len(r.urls) != 0 || strings.Contains(r.out, "github.com") {
				t.Fatalf("urls %v:\n%s", r.urls, r.out)
			}
		}})
	cases = append(cases, psCase{"version wins", map[string]string{"AIMEM_BASE": writeExport(t, goodManifest, exportTag, "v2.0.0") + `\`, "AIMEM_VERSION": "v2.0.0"},
		func(t *testing.T, r result) {
			if !installed(r, "v2.0.0") {
				t.Fatal(r.out)
			}
		}})
	cases = append(cases, psCase{"https export", map[string]string{"AIMEM_BASE": "https://export.test/x", "MAP_FROM": "https://export.test/x/", "MAP_TO": writeExport(t, goodManifest, exportTag)},
		func(t *testing.T, r result) {
			if !installed(r, exportTag) || len(r.urls) == 0 || r.urls[0] != "https://export.test/x/MANIFEST" {
				t.Fatalf("urls %v:\n%s", r.urls, r.out)
			}
			for _, u := range r.urls {
				if !strings.HasPrefix(u, "https://export.test/x/") {
					t.Errorf("contacted %s", u)
				}
			}
		}})
	gh := "https://github.com/o/r/releases/download/"
	cases = append(cases, psCase{"connected", map[string]string{"AIMEM_REPO": "o/r", "AIMEM_VERSION": exportTag, "MAP_FROM": gh, "MAP_TO": filepath.Join(writeExport(t, "", exportTag), "aimem")},
		func(t *testing.T, r result) {
			if !installed(r, exportTag) {
				t.Fatal(r.out)
			}
		}})

	// A release without installer assets hands over to its own boot.ps1,
	// run in this process with the same environment; in an export it is
	// refused.
	oldSums := "https://github.com/o/r/releases/download/" + olderTag + "/SHA256SUMS"
	oldScript := "https://raw.githubusercontent.com/o/r/" + olderTag + "/boot.ps1"
	cases = append(cases, psCase{"older release hands over", map[string]string{"AIMEM_REPO": "o/r", "AIMEM_VERSION": olderTag, "MAP_FROM": "https://",
		"MAP_TO": olderReleaseWeb(t, "boot.ps1", "Write-Output \"old boot ran: version=$env:AIMEM_VERSION repo=$env:AIMEM_REPO dir=$(Split-Path -Leaf $PWD.Path)\"\r\n")},
		func(t *testing.T, r result) {
			if !strings.Contains(r.out, "aimem "+olderTag+" has no installer assets; handing over to that release's own boot script ("+oldScript+")") ||
				!strings.Contains(r.out, "old boot ran: version="+olderTag+" repo=o/r dir=project") ||
				strings.Contains(r.out, "installer ran") || strings.Contains(r.out, "failure=") {
				t.Fatal(r.out)
			}
			if strings.Join(r.urls, " ") != oldSums+" "+oldScript {
				t.Errorf("fetched %v, want the sums then the old boot script", r.urls)
			}
		}})
	olderExport := filepath.Join(t.TempDir(), "export")
	writeOlderRelease(t, filepath.Join(olderExport, "aimem", olderTag))
	cases = append(cases, psCase{"older release in an export", map[string]string{"AIMEM_BASE": olderExport, "AIMEM_VERSION": olderTag},
		func(t *testing.T, r result) {
			if !strings.Contains(r.out, "/install.ps1 is missing") || !strings.Contains(r.out, "nothing was installed") ||
				strings.Contains(r.out, "handing over") || len(r.urls) != 0 {
				t.Fatalf("urls %v:\n%s", r.urls, r.out)
			}
		}})

	rel := func(base string) string { return filepath.Join(base, "aimem", exportTag) }
	for _, c := range []struct {
		name  string
		setup func() string
		want  string
	}{
		{"missing manifest", func() string { return writeExport(t, "", exportTag) }, `MANIFEST is missing (or set AIMEM_VERSION`},
		{"unknown format", func() string {
			return writeExport(t, "aiforge-export 2\naimem "+exportTag+"\n", exportTag)
		}, "MANIFEST is 'aiforge-export 2', not a format this installer reads"},
		{"no aimem line", func() string {
			return writeExport(t, "aiforge-export 1\naiskills v9.9.9\n", exportTag)
		}, "MANIFEST names no aimem release"},
		{"missing asset", func() string {
			b := writeExport(t, goodManifest, exportTag)
			if err := os.Remove(filepath.Join(rel(b), "install.ps1")); err != nil {
				t.Fatal(err)
			}
			return b
		}, "/install.ps1 is missing"},
		{"sum mismatch", func() string {
			b := writeExport(t, goodManifest, exportTag)
			if err := os.WriteFile(filepath.Join(rel(b), "aimem-windows-amd64.exe"), []byte("tampered"), 0o644); err != nil {
				t.Fatal(err)
			}
			return b
		}, "checksum mismatch for "},
		{"sums list only a longer name", func() string {
			b := writeExport(t, goodManifest, exportTag)
			sums := filepath.Join(rel(b), "SHA256SUMS")
			s, err := os.ReadFile(sums)
			if err != nil {
				t.Fatal(err)
			}
			s = []byte(strings.Replace(string(s), "  aimem-windows-amd64.exe", "  old-aimem-windows-amd64.exe", 1))
			if err := os.WriteFile(sums, s, 0o644); err != nil {
				t.Fatal(err)
			}
			return b
		}, "(want absent, got "},
		{"plain http", func() string { return "http://export.test" }, "AIMEM_BASE must be an absolute directory"},
		{"relative base", func() string { return `export\dir` }, "AIMEM_BASE must be an absolute directory"},
	} {
		want := c.want
		cases = append(cases, psCase{c.name, map[string]string{"AIMEM_BASE": c.setup()},
			func(t *testing.T, r result) {
				if !strings.Contains(r.out, want) || !strings.Contains(r.out, "nothing was installed") ||
					strings.Contains(r.out, "installer ran") || len(r.urls) != 0 {
					t.Fatalf("want %q, urls %v:\n%s", want, r.urls, r.out)
				}
			}})
	}

	// One script: the stub, then each case between markers, its variables
	// set before the run and removed after it.
	psq := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }
	var b strings.Builder
	b.WriteString(`function Invoke-WebRequest {
  param([Parameter(Position = 0)][string]$Uri, [string]$OutFile, [switch]$UseBasicParsing)
  Add-Content -LiteralPath $env:FETCH_LOG -Value $Uri
  if (-not $Uri.StartsWith($env:MAP_FROM)) { throw "unexpected host: $Uri" }
  $p = Join-Path $env:MAP_TO $Uri.Substring($env:MAP_FROM.Length)
  if (-not (Test-Path -LiteralPath $p -PathType Leaf)) { throw "404 $Uri" }
  Copy-Item -LiteralPath $p -Destination $OutFile
}
$boot = Get-Content -Raw -LiteralPath $env:BOOT
`)
	logs := make([]string, len(cases))
	for i, c := range cases {
		logs[i] = filepath.Join(dir, fmt.Sprintf("fetch-%d.log", i))
		env := map[string]string{"MAP_FROM": "https://nowhere.invalid/", "MAP_TO": dir, "FETCH_LOG": logs[i]}
		for k, v := range c.env {
			env[k] = v
		}
		fmt.Fprintf(&b, "Write-Output '=== case %d'\n", i)
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "$env:%s = %s\n", k, psq(env[k]))
		}
		b.WriteString("try { & ([scriptblock]::Create($boot)) } catch { Write-Output \"failure=[$_]\" }\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "Remove-Item Env:%s -ErrorAction SilentlyContinue\n", k)
		}
	}
	fmt.Fprintf(&b, "Write-Output '=== case %d'\n", len(cases))
	file := filepath.Join(dir, "run.ps1")
	if err := os.WriteFile(file, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	var e []string
	for _, kv := range withoutAimemEnv() {
		if strings.HasPrefix(strings.ToUpper(kv), "PSMODULEPATH=") && !strings.EqualFold(filepath.Base(ps), "pwsh.exe") {
			continue
		}
		e = append(e, kv)
	}
	out, _ := runPowerShell(t, ps, project, append(e, "BOOT="+absRepo(t, "boot.ps1")), file)

	for i, c := range cases {
		start := strings.Index(out, fmt.Sprintf("=== case %d", i))
		end := strings.Index(out, fmt.Sprintf("=== case %d", i+1))
		r := result{}
		if start >= 0 && end > start {
			r.out = out[start:end]
		}
		if l, err := os.ReadFile(logs[i]); err == nil {
			r.urls = strings.Fields(string(l))
		}
		t.Run(c.name, func(t *testing.T) {
			if r.out == "" {
				t.Fatalf("the case did not run to its end marker:\n%s", out)
			}
			c.check(t, r)
		})
	}
}
