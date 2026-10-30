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

// boot.ps1 run as `irm | iex` runs in the caller's own process, so the two
// variables it sets for install.ps1 must be put back as they were, even
// when the run fails: unset stays unset, and a value the user set stays
// theirs. The downloads are stubbed: the archive and the binary arrive as
// placeholder files and SHA256SUMS lists nothing, so the run gets past
// setting AIMEM_PREBUILT and fails on the checksum, offline. Get-FileHash
// is stubbed too: Windows PowerShell started by a test under a pwsh step
// inherits pwsh's module path and could not load it on the CI runner.
func TestBootPs1LeavesNoVariablesInTheCallersSession(t *testing.T) {
	shells := powerShells(t)
	boot, err := filepath.Abs(filepath.Join("..", "..", "boot.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, before, want string
	}{
		{"unset before", "", "target=[] prebuilt=[]"},
		{"set by the user", "$env:AIMEM_TARGET_VERSION = 'v0.0.1'; $env:AIMEM_PREBUILT = 'C:\\mine\\aimem.exe'\n", `target=[v0.0.1] prebuilt=[C:\mine\aimem.exe]`},
	} {
		for _, ps := range shells {
			t.Run(c.name+" in "+filepath.Base(ps), func(t *testing.T) {
				dir := t.TempDir()
				script := c.before + `function Invoke-WebRequest { param($Uri, $OutFile, [switch]$UseBasicParsing) Set-Content -LiteralPath $OutFile -Value "placeholder for $Uri" }
function tar { }
function Get-FileHash { [pscustomobject]@{ Hash = 'PLACEHOLDER' } }
$failure = ''
try { Get-Content -Raw -LiteralPath $env:BOOT | Invoke-Expression } catch { $failure = "$_" }
Write-Output "failure=[$failure]"
Write-Output "target=[$env:AIMEM_TARGET_VERSION] prebuilt=[$env:AIMEM_PREBUILT]"
`
				file := filepath.Join(dir, "session.ps1")
				if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
					t.Fatal(err)
				}
				out, err := runPowerShell(t, ps, dir, append(withoutAimemEnv(), "BOOT="+boot), file)
				if err != nil {
					t.Fatalf("%v:\n%s", err, out)
				}
				if !strings.Contains(out, "failure=[checksum mismatch") {
					t.Fatalf("the stubbed run did not reach the checksum, so AIMEM_PREBUILT was never set:\n%s", out)
				}
				if !strings.Contains(out, c.want) {
					t.Fatalf("the session holds the run's variables; want %q:\n%s", c.want, out)
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
	guides := []string{"README.md", "docs/ADMIN-MANUAL.md", "docs/INSTALL-CLIENT.md", "docs/QUICKSTART.md", "docs/QUICKSTART-NEW-PROJECT.md"}
	lines, blocks := map[string]string{}, map[string]string{}
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

	dir := t.TempDir()
	stub := filepath.Join(dir, "release")
	project := filepath.Join(dir, "project")
	for _, d := range []string{stub, project, filepath.Join(dir, "temp")} {
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
	boot := []byte("if ($PSCommandPath) { $null = Get-FileHash -LiteralPath $PSCommandPath }\r\nWrite-Output \"run saw=[$env:AIMEM_USER_ONLY] cwd=[$($PWD.Path)]\"\r\nif ($env:STUB_FAIL) { throw 'the stub run failed' }\r\n")
	if err := os.WriteFile(filepath.Join(stub, "boot.ps1"), boot, 0o600); err != nil {
		t.Fatal(err)
	}
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
			if strings.HasPrefix(k, "TEMP=") || strings.HasPrefix(k, "STUB_FAIL=") || (strings.HasPrefix(k, "PSMODULEPATH=") && !strings.EqualFold(filepath.Base(ps), "pwsh.exe")) {
				continue
			}
			env = append(env, kv)
		}
		return append(append(env, "STUB="+stub, "TEMP="+filepath.Join(dir, "temp")), extra...)
	}

	run := func(t *testing.T, ps, block, sums string, extra ...string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(stub, "SHA256SUMS"), []byte(sums), 0o600); err != nil {
			t.Fatal(err)
		}
		block = download.ReplaceAllString(block, copyMarked)
		block = oneLine.ReplaceAllString(block, fetch)
		var lines []string
		for _, l := range strings.Split(block, "\n") {
			if !strings.HasPrefix(l, "cd ") {
				lines = append(lines, l)
			}
		}
		script := "try {\n" + strings.Join(lines, "\n") + "\n} catch { Write-Output \"failure=[$_]\" }\nWrite-Output \"window after=[$env:AIMEM_USER_ONLY]\"\n"
		file := filepath.Join(dir, "window.ps1")
		if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		out, _ := runPowerShell(t, ps, project, envFor(ps, extra...), file)
		return out
	}
	want := func(block string) string {
		if strings.Contains(block, "AIMEM_USER_ONLY") {
			return "run saw=[1] cwd=[" + project + "]"
		}
		return "run saw=[] cwd=[" + project + "]"
	}
	for key, line := range lines {
		doc := strings.SplitN(key, "\n", 2)[0]
		for _, ps := range shells {
			t.Run("line in "+doc+" in "+filepath.Base(ps), func(t *testing.T) {
				out := run(t, ps, line, good)
				if !strings.Contains(out, want(line)) || strings.Contains(out, "failure=") {
					t.Fatalf("the line did not run boot.ps1 from the caller's directory with %q:\n%s", want(line), out)
				}
				if !strings.Contains(out, "window after=[]") {
					t.Fatalf("the line left AIMEM_USER_ONLY set in the window:\n%s", out)
				}
				out = run(t, ps, line, good, "STUB_FAIL=1")
				if !strings.Contains(out, "the stub run failed") || !strings.Contains(out, "window after=[]") {
					t.Fatalf("a failed run left AIMEM_USER_ONLY set in the window:\n%s", out)
				}
			})
		}
	}
	for key, block := range blocks {
		doc := strings.SplitN(key, "\n", 2)[0]
		for _, ps := range shells {
			t.Run("block in "+doc+" in "+filepath.Base(ps), func(t *testing.T) {
				out := run(t, ps, block, good)
				if !strings.Contains(out, want(block)) {
					t.Fatalf("the block did not run boot.ps1 from the caller's directory with %q:\n%s", want(block), out)
				}
				if !strings.Contains(out, "window after=[]") {
					t.Fatalf("the block left AIMEM_USER_ONLY set in the window:\n%s", out)
				}
				out = run(t, ps, block, tampered)
				if strings.Contains(out, "run saw=") || !strings.Contains(out, "boot.ps1 does not match SHA256SUMS") {
					t.Fatalf("the block ran a boot.ps1 whose hash SHA256SUMS does not list:\n%s", out)
				}
				out = run(t, ps, strings.Replace(block, "  Unblock-File -LiteralPath $f\n", "", 1), good)
				if strings.Contains(out, "run saw=") {
					t.Fatalf("a boot.ps1 marked as downloaded ran under RemoteSigned without Unblock-File, so the stub's mark does not test it:\n%s", out)
				}
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
