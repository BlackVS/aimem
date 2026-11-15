package installer

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// boot.ps1 run as `irm | iex` runs in the caller's own process and scope,
// so it must leave the session as it found it, after a failed run and
// after a successful one. The two environment variables it sets for
// install.ps1 are put back (unset stays unset, and a value the user set
// stays theirs), and so is the process-wide TLS setting. Its own
// variables, $ErrorActionPreference = 'Stop' among them, never reach the
// caller's scope. The downloads are stubbed: every asset arrives as a
// placeholder file. A failed run's SHA256SUMS lists only install.ps1 (a
// release with no installer listed would hand over to its own boot
// script), so the run gets past setting AIMEM_TARGET_VERSION and fails on
// the binary's checksum, offline. A successful run's lists the placeholder hash for each asset,
// and the installer's shell is stubbed to succeed.
// Get-FileHash is stubbed too: Windows PowerShell started by a test under
// a pwsh step inherits pwsh's module path and could not load it on the CI
// runner. It is stubbed through an alias: Get-FileHash is itself a module
// function, and under Invoke-Expression the module's copy wins over a
// function of the same name, while an alias outranks both.
func TestBootPs1LeavesNoVariablesInTheCallersSession(t *testing.T) {
	shells := powerShells(t)
	boot, err := filepath.Abs(filepath.Join("..", "..", "boot.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	// The stubs of a successful run: SHA256SUMS lists the placeholder
	// hash for every asset, and the installer succeeds.
	const succeeds = `function Invoke-WebRequest { [CmdletBinding()] param([Parameter(Position = 0)]$Uri, $OutFile, [switch]$UseBasicParsing, [switch]$PassThru, $MaximumRedirection, [switch]$SkipHttpErrorCheck)
  if ($OutFile -like '*SHA256SUMS') { Set-Content -LiteralPath $OutFile -Value 'placeholder  aimem-windows-amd64.exe', 'placeholder  install.ps1', 'placeholder  aimem-opencode.ts' } else { Set-Content -LiteralPath $OutFile -Value "placeholder for $Uri" }
  if ($PassThru) { [pscustomobject]@{ StatusCode = 200; Headers = @{} } } }
function powershell { $global:LASTEXITCODE = 0 }
`
	const fails = `function Invoke-WebRequest { [CmdletBinding()] param([Parameter(Position = 0)]$Uri, $OutFile, [switch]$UseBasicParsing, [switch]$PassThru, $MaximumRedirection, [switch]$SkipHttpErrorCheck)
  if ($OutFile -like '*SHA256SUMS') { Set-Content -LiteralPath $OutFile -Value 'placeholder  install.ps1' } else { Set-Content -LiteralPath $OutFile -Value "placeholder for $Uri" }
  if ($PassThru) { [pscustomobject]@{ StatusCode = 200; Headers = @{} } } }
`
	for _, c := range []struct {
		name, before, stubs, failure, want string
	}{
		{"unset before, failed run", "", fails, "failure=[checksum mismatch", "target=[] prebuilt=[]"},
		{"set by the user, failed run", "$env:AIMEM_TARGET_VERSION = 'v0.0.1'; $env:AIMEM_PREBUILT = 'C:\\mine\\aimem.exe'\n", fails, "failure=[checksum mismatch", `target=[v0.0.1] prebuilt=[C:\mine\aimem.exe]`},
		{"unset before, successful run", "", succeeds, "failure=[]", "target=[] prebuilt=[]"},
	} {
		for _, ps := range shells {
			t.Run(c.name+" in "+filepath.Base(ps), func(t *testing.T) {
				dir := t.TempDir()
				script := c.before + c.stubs + `function Get-PlaceholderHash { [pscustomobject]@{ Hash = 'PLACEHOLDER' } }
Set-Alias Get-FileHash Get-PlaceholderHash
$protocol = [Net.ServicePointManager]::SecurityProtocol
$failure = ''
try { Get-Content -Raw -LiteralPath $env:BOOT | Invoke-Expression } catch { $failure = "$_" }
Write-Output "failure=[$failure]"
Write-Output "target=[$env:AIMEM_TARGET_VERSION] prebuilt=[$env:AIMEM_PREBUILT]"
Write-Output "preference=[$ErrorActionPreference]"
Write-Output "leaked=[$(@('release', 'tag', 'saved', 'dest', 'repo', 'source', 'src', 'prebuilt', 'installer') | Where-Object { Get-Variable $_ -ErrorAction SilentlyContinue })]"
Write-Output "protocol kept=[$([Net.ServicePointManager]::SecurityProtocol -eq $protocol)]"
`
				file := filepath.Join(dir, "session.ps1")
				if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
					t.Fatal(err)
				}
				out, err := runPowerShell(t, ps, dir, append(withoutAimemEnv(), "BOOT="+boot), file)
				if err != nil {
					t.Fatalf("%v:\n%s", err, out)
				}
				if !strings.Contains(out, c.failure) {
					t.Fatalf("the stubbed run did not end as planned (want %q), so it did not set AIMEM_PREBUILT and run to the end:\n%s", c.failure, out)
				}
				if !strings.Contains(out, c.want) {
					t.Fatalf("the session holds the run's variables; want %q:\n%s", c.want, out)
				}
				for _, w := range []string{"preference=[Continue]", "leaked=[]", "protocol kept=[True]"} {
					if !strings.Contains(out, w) {
						t.Fatalf("the run left its state in the caller's session; want %q:\n%s", w, out)
					}
				}
			})
		}
	}
}

// Scenario test for the documented Windows install: every install guide
// shows the one line `irm <release>/boot.ps1 | iex`, and INSTALL-CLIENT
// alone keeps the optional stricter block. Run from a PowerShell window,
// the line runs boot.ps1 in that window from the caller's directory, and
// its knob form hands the knob only to the run, even when the run fails.
// The block checks boot.ps1 against SHA256SUMS before it runs it, runs it
// as a file under RemoteSigned, and runs nothing when the hash does not
// match. The downloads are replaced by a stub boot.ps1 and its SHA256SUMS;
// the block's copies are marked as coming from the internet the way
// PowerShell 7's Invoke-WebRequest marks them, so its Unblock-File is
// exercised too.
func TestDocumentedWindowsInstallRunsBootPs1(t *testing.T) {
	shells := powerShells(t)
	oneLine := regexp.MustCompile(`irm https://github\.com/BlackVS/aimem/releases/download/[^/\s]+/boot\.ps1 \| iex`)
	download := regexp.MustCompile(`Invoke-WebRequest https://github\.com/BlackVS/aimem/releases/download/[^/\s]+/(boot\.ps1|SHA256SUMS) -OutFile (\$\w+) -UseBasicParsing`)
	share := regexp.MustCompile(`\$env:AIMEM_BASE = '\\\\[^']+'`)
	guides := []string{"README.md", "docs/ADMIN-MANUAL.md", "docs/INSTALL-CLIENT.md", "docs/QUICKSTART.md", "docs/QUICKSTART-NEW-PROJECT.md"}
	lines, blocks, exports := map[string]string{}, map[string]string{}, map[string]string{}
	for _, doc := range guides {
		text := read(t, doc)
		found := 0
		for _, m := range regexp.MustCompile("(?s)```(\\w*)\n(.*?)```").FindAllStringSubmatch(text, -1) {
			if !strings.Contains(m[2], "boot.ps1") || strings.Contains(m[2], "install.ps1") {
				continue
			}
			if strings.Contains(m[2], "Bypass") {
				t.Errorf("%s documents a boot.ps1 form with a policy bypass:\n%s", doc, m[2])
			}
			if m[1] != "powershell" {
				continue
			}
			switch {
			case oneLine.MatchString(m[2]):
				found++
				lines[doc+"\n"+m[2]] = m[2]
			case len(download.FindAllString(m[2], -1)) == 2:
				if doc != "docs/INSTALL-CLIENT.md" {
					t.Errorf("%s keeps the stricter block; only INSTALL-CLIENT does, after the one line:\n%s", doc, m[2])
				}
				blocks[doc+"\n"+m[2]] = m[2]
			case share.MatchString(m[2]):
				if doc != "docs/ADMIN-MANUAL.md" {
					t.Errorf("%s shows the export form; only ADMIN-MANUAL does:\n%s", doc, m[2])
				}
				exports[doc+"\n"+m[2]] = m[2]
			default:
				t.Errorf("%s has a boot.ps1 block that is neither the one line nor the checked block:\n%s", doc, m[2])
			}
		}
		if found == 0 {
			t.Errorf("%s shows no Windows one-liner", doc)
		}
	}
	if len(blocks) != 1 {
		t.Fatalf("found %d stricter blocks; want the one in INSTALL-CLIENT", len(blocks))
	}
	if len(exports) != 1 {
		t.Fatalf("found %d export forms; want the one in ADMIN-MANUAL", len(exports))
	}

	dir := t.TempDir()
	project := filepath.Join(dir, "project")
	for _, d := range []string{project, filepath.Join(dir, "temp")} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// The runner's temp directory is a short 8.3 name (RUNNER~1), and
	// PowerShell reports the working directory by its long name.
	project, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	// Run as a file, the stub loads Get-FileHash, as the real boot.ps1
	// does, so the child Windows PowerShell has to load its own utility
	// module. STUB_FAIL makes it fail after it reports what it saw.
	boot := []byte("if ($PSCommandPath) { $null = Get-FileHash -LiteralPath $PSCommandPath }\r\nWrite-Output \"run saw=[$env:AIMEM_USER_ONLY] cwd=[$($PWD.Path)] base=[$env:AIMEM_BASE]\"\r\nif ($env:STUB_FAIL) { throw 'the stub run failed' }\r\n")
	// SHA256SUMS carries lower-case hashes and Get-FileHash answers in
	// upper case; the block's -ne compares them case-insensitively on
	// purpose.
	sum := sha256.Sum256(boot)
	good := fmt.Sprintf("%x  aimem-windows-amd64.exe\n%x  boot.ps1\n", sha256.Sum256([]byte("binary")), sum)
	tampered := fmt.Sprintf("%x  boot.ps1\n", sha256.Sum256([]byte("another boot.ps1")))
	copyMarked := `Copy-Item -LiteralPath (Join-Path $$env:STUB '${1}') -Destination ${2}; Set-Content -LiteralPath ${2} -Stream Zone.Identifier -Value '[ZoneTransfer]', 'ZoneId=3'`
	fetch := `Get-Content -Raw -LiteralPath (Join-Path $$env:STUB 'boot.ps1') | iex`
	// A Windows PowerShell window started by this test under a pwsh step
	// would inherit pwsh's module path and could not load Get-FileHash; a
	// user's window has its own shell's path. A pwsh window keeps it, so
	// the child powershell it starts gets what a pwsh user's child gets.
	envFor := func(ps string, extra ...string) []string {
		var env []string
		for _, kv := range withoutAimemEnv() {
			k := strings.ToUpper(kv)
			if strings.HasPrefix(k, "TEMP=") || strings.HasPrefix(k, "STUB=") || strings.HasPrefix(k, "STUB_FAIL=") || (strings.HasPrefix(k, "PSMODULEPATH=") && !strings.EqualFold(filepath.Base(ps), "pwsh.exe")) {
				continue
			}
			env = append(env, kv)
		}
		return append(append(env, "TEMP="+filepath.Join(dir, "temp")), extra...)
	}

	// A run is one window: the documented code with its downloads replaced,
	// against a stub folder of its own (boot.ps1 and the SHA256SUMS the
	// case wants), with STUB_FAIL set when the stub should fail.
	type run struct {
		code, sums, tag string // tag: the export release the stub folder holds
		stale           string // a file left in TEMP before the run, as by an interrupted one
		fail            bool
	}
	prepare := func(code string) string {
		code = download.ReplaceAllString(code, copyMarked)
		code = oneLine.ReplaceAllString(code, fetch)
		code = share.ReplaceAllString(code, `$$env:AIMEM_BASE = $$env:STUB`)
		var kept []string
		for _, l := range strings.Split(code, "\n") {
			if !strings.HasPrefix(l, "cd ") {
				kept = append(kept, l)
			}
		}
		return strings.Join(kept, "\n")
	}
	// All runs of one shell go into one PowerShell process, one after
	// another between markers, as TestBootPs1InstallsFromAnExport does: a
	// Windows PowerShell start costs about 18 s on the CI runner, and a
	// process per run made this the package's slowest test. Each run sets
	// its own STUB and STUB_FAIL and reports the window's AIMEM_USER_ONLY
	// after it; the variables are removed before the next run, so a leak
	// shows in the run that leaked it.
	batch := func(t *testing.T, ps string, runs []run) []string {
		t.Helper()
		var b strings.Builder
		for i, r := range runs {
			own := filepath.Join(dir, fmt.Sprintf("release-%s-%d", strings.TrimSuffix(filepath.Base(ps), ".exe"), i))
			if err := os.Mkdir(own, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(own, "boot.ps1"), boot, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(own, "SHA256SUMS"), []byte(r.sums), 0o600); err != nil {
				t.Fatal(err)
			}
			if r.tag != "" {
				rel := filepath.Join(own, "aimem", r.tag)
				if err := os.MkdirAll(rel, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(rel, "boot.ps1"), boot, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			fmt.Fprintf(&b, "Write-Output '=== run %d'\n$env:STUB = '%s'\n", i, strings.ReplaceAll(own, "'", "''"))
			if r.fail {
				b.WriteString("$env:STUB_FAIL = '1'\n")
			}
			if r.stale != "" {
				fmt.Fprintf(&b, "Set-Content -LiteralPath (Join-Path $env:TEMP '%s') -Value 'Write-Output \"stale copy ran\"'\n", r.stale)
			}
			b.WriteString("try {\n" + prepare(r.code) + "\n} catch { Write-Output \"failure=[$_]\" }\n")
			b.WriteString("Write-Output \"window after=[$env:AIMEM_USER_ONLY] base after=[$env:AIMEM_BASE] temp after=[$((Get-ChildItem -LiteralPath $env:TEMP -Filter *.ps1).Name -join ',')]\"\n")
			b.WriteString("Remove-Item Env:STUB, Env:STUB_FAIL, Env:AIMEM_USER_ONLY, Env:AIMEM_BASE -ErrorAction SilentlyContinue\n")
		}
		fmt.Fprintf(&b, "Write-Output '=== run %d'\n", len(runs))
		file := filepath.Join(dir, "window-"+strings.TrimSuffix(filepath.Base(ps), ".exe")+".ps1")
		if err := os.WriteFile(file, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		out, _ := runPowerShell(t, ps, project, envFor(ps), file)
		outs := make([]string, len(runs))
		for i := range runs {
			start := strings.Index(out, fmt.Sprintf("=== run %d", i))
			end := strings.Index(out, fmt.Sprintf("=== run %d", i+1))
			if start < 0 || end <= start {
				t.Fatalf("run %d did not reach its end marker:\n%s", i, out)
			}
			outs[i] = out[start:end]
		}
		return outs
	}
	want := func(code string) string {
		if strings.Contains(code, "AIMEM_USER_ONLY") {
			return "run saw=[1] cwd=[" + project + "]"
		}
		return "run saw=[] cwd=[" + project + "]"
	}

	// Each documented line runs twice (the stub succeeds, then fails) and
	// the block three times (a good hash, a tampered one, and without its
	// Unblock-File); the checks below read each run's output by its index.
	type check struct {
		name string
		at   int
		fn   func(t *testing.T, outs []string)
	}
	var runs []run
	var checks []check
	for key, line := range lines {
		doc := strings.SplitN(key, "\n", 2)[0]
		at := len(runs)
		runs = append(runs, run{line, good, "", "", false}, run{line, good, "", "", true})
		checks = append(checks, check{"line in " + doc, at, func(t *testing.T, outs []string) {
			if out := outs[0]; !strings.Contains(out, want(line)) || strings.Contains(out, "failure=") {
				t.Fatalf("the line did not run boot.ps1 from the caller's directory with %q:\n%s", want(line), out)
			} else if !strings.Contains(out, "window after=[]") {
				t.Fatalf("the line left AIMEM_USER_ONLY set in the window:\n%s", out)
			}
			if out := outs[1]; !strings.Contains(out, "the stub run failed") || !strings.Contains(out, "window after=[]") {
				t.Fatalf("a failed run left AIMEM_USER_ONLY set in the window:\n%s", out)
			}
		}})
	}
	for key, block := range blocks {
		doc := strings.SplitN(key, "\n", 2)[0]
		at := len(runs)
		runs = append(runs, run{block, good, "", "", false}, run{block, tampered, "", "", false},
			run{strings.Replace(block, "  Unblock-File -LiteralPath $f\n", "", 1), good, "", "", false})
		checks = append(checks, check{"block in " + doc, at, func(t *testing.T, outs []string) {
			if out := outs[0]; !strings.Contains(out, want(block)) {
				t.Fatalf("the block did not run boot.ps1 from the caller's directory with %q:\n%s", want(block), out)
			} else if !strings.Contains(out, "window after=[]") {
				t.Fatalf("the block left AIMEM_USER_ONLY set in the window:\n%s", out)
			}
			if out := outs[1]; strings.Contains(out, "run saw=") || !strings.Contains(out, "boot.ps1 does not match SHA256SUMS") {
				t.Fatalf("the block ran a boot.ps1 whose hash SHA256SUMS does not list:\n%s", out)
			}
			if out := outs[2]; strings.Contains(out, "run saw=") {
				t.Fatalf("a boot.ps1 marked as downloaded ran under RemoteSigned without Unblock-File, so the stub's mark does not test it:\n%s", out)
			}
		}})
	}
	// The export form copies boot.ps1 from the share to a local folder and
	// runs the copy as a file: the run sees the share as its base and the
	// caller's directory, and the window keeps neither the variable nor
	// the copy, even when the run fails. A copy that fails stops the form:
	// a copy an interrupted earlier run left in TEMP does not run, and is
	// removed with the variable. Each run reports what TEMP holds after it.
	tagIn := regexp.MustCompile(`\\aimem\\(v[^\\]+)\\boot\.ps1`)
	copyIn := regexp.MustCompile(`Join-Path \$env:TEMP '([^']+\.ps1)'`)
	for key, form := range exports {
		doc := strings.SplitN(key, "\n", 2)[0]
		tag, copied := tagIn.FindStringSubmatch(form), copyIn.FindStringSubmatch(form)
		if tag == nil || copied == nil {
			t.Fatalf("%s: the export form names no aimem\\<tag>\\boot.ps1 or no copy in TEMP:\n%s", doc, form)
		}
		at := len(runs)
		runs = append(runs, run{form, good, tag[1], "", false}, run{form, good, tag[1], "", true}, run{form, good, "", copied[1], false})
		checks = append(checks, check{"export form in " + doc, at, func(t *testing.T, outs []string) {
			for i, out := range outs[:3] {
				if !strings.Contains(out, "base after=[] temp after=[]") {
					t.Fatalf("run %d left AIMEM_BASE or a copy in the window:\n%s", i, out)
				}
			}
			for i, out := range outs[:2] {
				if !strings.Contains(out, "run saw=[] cwd=["+project+"] base=[") || strings.Contains(out, "base=[]") {
					t.Fatalf("run %d did not run boot.ps1 from the caller's directory with the export as its base:\n%s", i, out)
				}
			}
			if strings.Contains(outs[0], "failure=") {
				t.Fatalf("the export form failed:\n%s", outs[0])
			}
			if !strings.Contains(outs[1], "the stub run failed") {
				t.Fatalf("the failing stub did not run:\n%s", outs[1])
			}
			if out := outs[2]; strings.Contains(out, "stale copy ran") || strings.Contains(out, "run saw=") || !strings.Contains(out, "failure=") {
				t.Fatalf("a failed copy did not stop the form before a stale copy ran:\n%s", out)
			}
		}})
	}
	for _, ps := range shells {
		outs := batch(t, ps, runs)
		for _, c := range checks {
			t.Run(c.name+" in "+filepath.Base(ps), func(t *testing.T) {
				c.fn(t, outs[c.at:])
			})
		}
	}
}

// powerShells is every PowerShell on the PATH, so a run covers both
// Windows PowerShell 5.1 and PowerShell 7 where the machine has them.
func powerShells(t *testing.T) []string {
	t.Helper()
	powerShell(t)
	var found []string
	for _, name := range []string{"powershell", "pwsh"} {
		if p, err := exec.LookPath(name); err == nil {
			found = append(found, p)
		}
	}
	return found
}

// withoutAimemEnv is the test's environment without any AIMEM_ variable,
// so a knob the developer's own session holds cannot pass or fail a test.
func withoutAimemEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "AIMEM_") {
			env = append(env, kv)
		}
	}
	return env
}
