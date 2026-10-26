package installer

import (
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
// setting AIMEM_PREBUILT and fails on the checksum, offline.
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

// Scenario test for the documented Windows knob forms: each one in
// ADMIN-MANUAL, run from the shell it is written for, hands the knob to
// the run and leaves the caller's window without it. The download is
// replaced by a stub that reports the knob it saw.
func TestDocumentedWindowsKnobFormsScopeTheKnobToTheRun(t *testing.T) {
	shells := powerShells(t)
	manual := read(t, filepath.Join("docs", "ADMIN-MANUAL.md"))
	fetch := regexp.MustCompile(`irm https://\S+/boot\.ps1 \| iex`)
	forms := map[string]string{}
	for _, lang := range []string{"powershell", "bat"} {
		for _, m := range regexp.MustCompile("(?s)```"+lang+"\n(.*?)```").FindAllStringSubmatch(manual, -1) {
			if strings.Contains(m[1], "AIMEM_USER_ONLY") {
				if forms[lang] != "" {
					t.Fatalf("ADMIN-MANUAL has more than one %s knob form; extend this test", lang)
				}
				forms[lang] = strings.TrimSpace(m[1])
			}
		}
		if forms[lang] == "" {
			t.Fatalf("ADMIN-MANUAL has no %s form that passes AIMEM_USER_ONLY", lang)
		}
		if !fetch.MatchString(forms[lang]) {
			t.Fatalf("the %s form does not fetch boot.ps1 with irm | iex: %s", lang, forms[lang])
		}
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "stub.ps1")
	if err := os.WriteFile(stub, []byte("Write-Output \"run saw=[$env:AIMEM_USER_ONLY]\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	local := "Get-Content -Raw -LiteralPath $env:STUB | iex"
	env := append(withoutAimemEnv(), "STUB="+stub)

	psFile := filepath.Join(dir, "window.ps1")
	psScript := fetch.ReplaceAllLiteralString(forms["powershell"], local) + "\nWrite-Output \"window after=[$env:AIMEM_USER_ONLY]\"\n"
	if err := os.WriteFile(psFile, []byte(psScript), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, ps := range shells {
		out, err := runPowerShell(t, ps, dir, env, psFile)
		checkKnobScoped(t, "PowerShell window ("+filepath.Base(ps)+")", out, err)
	}

	batFile := filepath.Join(dir, "window.bat")
	bat := "@echo off\r\n" + fetch.ReplaceAllLiteralString(forms["bat"], local) + "\r\nif defined AIMEM_USER_ONLY (echo window after=[%AIMEM_USER_ONLY%]) else (echo window after=[])\r\n"
	if err := os.WriteFile(batFile, []byte(bat), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("cmd", "/d", "/c", batFile)
	cmd.Dir, cmd.Env = dir, append(env, "PSModuleAnalysisCachePath="+filepath.Join(dir, "ps-module-analysis-cache"))
	raw, err := cmd.CombinedOutput()
	checkKnobScoped(t, "cmd window", string(raw), err)
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

func checkKnobScoped(t *testing.T, shell, out string, err error) {
	t.Helper()
	if err != nil || !strings.Contains(out, "run saw=[1]") {
		t.Fatalf("%s: the documented form did not hand AIMEM_USER_ONLY=1 to the run (%v):\n%s", shell, err, out)
	}
	if !strings.Contains(out, "window after=[]") {
		t.Fatalf("%s: the documented form left AIMEM_USER_ONLY set in the window:\n%s", shell, out)
	}
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
