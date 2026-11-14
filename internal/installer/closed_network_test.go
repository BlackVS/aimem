package installer

// The installers read a release from an aiforge export (format 1) the same
// way they read it from github.com: <base>/MANIFEST names the release,
// <base>/aimem/<tag>/ holds its assets with SHA256SUMS, every file is
// checked before it is used, and with a base no other host is contacted.

import (
	"crypto/sha256"
	"encoding/hex"
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
// same export, never from github.com.
func TestClientHintNamesTheExport(t *testing.T) {
	shellOnly(t)
	t.Setenv("TAG", exportTag)
	for base, want := range map[string]string{
		"https://export.test/x/": "curl -fsSL https://export.test/x/aimem/" + exportTag + "/boot.sh | AIMEM_BASE=https://export.test/x bash\n" +
			"    Windows:      $env:AIMEM_BASE='https://export.test/x'; try { irm https://export.test/x/aimem/" + exportTag + "/boot.ps1 | iex } finally { Remove-Item Env:AIMEM_BASE }",
		"/media/export": "AIMEM_BASE=/media/export bash /media/export/aimem/" + exportTag + "/boot.sh",
	} {
		t.Setenv("AIMEM_BASE", base)
		out, errs := runClientHint(t, "", "https")
		if !strings.Contains(out, want) || strings.Contains(out+errs, "github") {
			t.Errorf("base %s: want %q in:\n%s%s", base, want, out, errs)
		}
	}
}

// boot.ps1 reads an export with the same rules: the release MANIFEST
// names, each file checked against SHA256SUMS on its whole name, the
// installer unblocked after its check (a copy from a downloaded export
// carries the Mark of the Web), nothing installed on any problem, and with
// a base no other host contacted. Invoke-WebRequest is a stub that logs
// each URL and serves those under MAP_FROM from MAP_TO.
func TestBootPs1InstallsFromAnExport(t *testing.T) {
	ps := powerShell(t)
	if runtime.GOOS != "windows" {
		t.Skip("boot.ps1 runs on Windows")
	}
	wrapper := `function Invoke-WebRequest {
  param([Parameter(Position = 0)][string]$Uri, [string]$OutFile, [switch]$UseBasicParsing)
  Add-Content -LiteralPath $env:FETCH_LOG -Value $Uri
  if (-not $Uri.StartsWith($env:MAP_FROM)) { throw "unexpected host: $Uri" }
  $p = Join-Path $env:MAP_TO $Uri.Substring($env:MAP_FROM.Length)
  if (-not (Test-Path -LiteralPath $p -PathType Leaf)) { throw "404 $Uri" }
  Copy-Item -LiteralPath $p -Destination $OutFile
}
try { & ([scriptblock]::Create((Get-Content -Raw -LiteralPath $env:BOOT))) } catch { Write-Output "failure=[$_]" }
`
	type result struct {
		out  string
		urls []string
	}
	run := func(t *testing.T, env ...string) result {
		t.Helper()
		dir := t.TempDir()
		project := filepath.Join(dir, "project")
		if err := os.Mkdir(project, 0o700); err != nil {
			t.Fatal(err)
		}
		project, err := filepath.EvalSymlinks(project)
		if err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, "run.ps1")
		if err := os.WriteFile(file, []byte(wrapper), 0o600); err != nil {
			t.Fatal(err)
		}
		log := filepath.Join(dir, "fetch.log")
		var e []string
		for _, kv := range withoutAimemEnv() {
			if strings.HasPrefix(strings.ToUpper(kv), "PSMODULEPATH=") && !strings.EqualFold(filepath.Base(ps), "pwsh.exe") {
				continue
			}
			e = append(e, kv)
		}
		e = append(e, "BOOT="+absRepo(t, "boot.ps1"), "FETCH_LOG="+log,
			"MAP_FROM=https://nowhere.invalid/", "MAP_TO="+dir)
		out, _ := runPowerShell(t, ps, project, append(e, env...), file)
		var urls []string
		if l, err := os.ReadFile(log); err == nil {
			urls = strings.Fields(string(l))
		}
		return result{out, urls}
	}
	installed := func(r result, tag string) bool {
		return strings.Contains(r.out, "installer ran: bootstrap=True tag="+tag) &&
			strings.Contains(r.out, "binary: bin-"+tag) && strings.Contains(r.out, "plugin: plugin-"+tag) &&
			!strings.Contains(r.out, "failure=")
	}

	t.Run("directory export, the installer marked as downloaded", func(t *testing.T) {
		base := writeExport(t, strings.ReplaceAll(goodManifest, "\n", "\r\n"), exportTag)
		inst := filepath.Join(base, "aimem", exportTag, "install.ps1")
		if err := os.WriteFile(inst+":Zone.Identifier", []byte("[ZoneTransfer]\r\nZoneId=3\r\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		r := run(t, "AIMEM_BASE="+base)
		if !installed(r, exportTag) || len(r.urls) != 0 || strings.Contains(r.out, "github.com") {
			t.Fatalf("urls %v:\n%s", r.urls, r.out)
		}
	})
	t.Run("version wins", func(t *testing.T) {
		base := writeExport(t, goodManifest, exportTag, "v2.0.0")
		if r := run(t, "AIMEM_BASE="+base+`\`, "AIMEM_VERSION=v2.0.0"); !installed(r, "v2.0.0") {
			t.Fatal(r.out)
		}
	})
	t.Run("https export", func(t *testing.T) {
		base := writeExport(t, goodManifest, exportTag)
		r := run(t, "AIMEM_BASE=https://export.test/x", "MAP_FROM=https://export.test/x/", "MAP_TO="+base)
		if !installed(r, exportTag) || len(r.urls) == 0 || r.urls[0] != "https://export.test/x/MANIFEST" {
			t.Fatalf("urls %v:\n%s", r.urls, r.out)
		}
		for _, u := range r.urls {
			if !strings.HasPrefix(u, "https://export.test/x/") {
				t.Errorf("contacted %s", u)
			}
		}
	})
	t.Run("connected", func(t *testing.T) {
		base := writeExport(t, "", exportTag)
		gh := "https://github.com/o/r/releases/download/"
		r := run(t, "AIMEM_REPO=o/r", "AIMEM_VERSION="+exportTag, "MAP_FROM="+gh, "MAP_TO="+filepath.Join(base, "aimem"))
		if !installed(r, exportTag) {
			t.Fatal(r.out)
		}
	})

	rel := func(base string) string { return filepath.Join(base, "aimem", exportTag) }
	for _, c := range []struct {
		name  string
		setup func(t *testing.T) string
		want  string
	}{
		{"missing manifest", func(t *testing.T) string { return writeExport(t, "", exportTag) }, `MANIFEST is missing (or set AIMEM_VERSION`},
		{"unknown format", func(t *testing.T) string {
			return writeExport(t, "aiforge-export 2\naimem "+exportTag+"\n", exportTag)
		}, "MANIFEST is 'aiforge-export 2', not a format this installer reads"},
		{"no aimem line", func(t *testing.T) string {
			return writeExport(t, "aiforge-export 1\naiskills v9.9.9\n", exportTag)
		}, "MANIFEST names no aimem release"},
		{"missing asset", func(t *testing.T) string {
			b := writeExport(t, goodManifest, exportTag)
			if err := os.Remove(filepath.Join(rel(b), "install.ps1")); err != nil {
				t.Fatal(err)
			}
			return b
		}, "/install.ps1 is missing"},
		{"sum mismatch", func(t *testing.T) string {
			b := writeExport(t, goodManifest, exportTag)
			if err := os.WriteFile(filepath.Join(rel(b), "aimem-windows-amd64.exe"), []byte("tampered"), 0o644); err != nil {
				t.Fatal(err)
			}
			return b
		}, "checksum mismatch for "},
		{"sums list only a longer name", func(t *testing.T) string {
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
		{"plain http", func(t *testing.T) string { return "http://export.test" }, "AIMEM_BASE must be an absolute directory"},
		{"relative base", func(t *testing.T) string { return `export\dir` }, "AIMEM_BASE must be an absolute directory"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := run(t, "AIMEM_BASE="+c.setup(t))
			if !strings.Contains(r.out, c.want) || !strings.Contains(r.out, "nothing was installed") ||
				strings.Contains(r.out, "installer ran") || len(r.urls) != 0 {
				t.Fatalf("want %q, urls %v:\n%s", c.want, r.urls, r.out)
			}
		})
	}
}
