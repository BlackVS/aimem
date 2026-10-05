package installps

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// extract returns the installer's text between its BEGIN and END markers
// for one function.
func extract(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "install.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	begin, end := "# BEGIN "+name+"\n", "# END "+name
	i, j := strings.Index(s, begin), strings.Index(s, end)
	if i < 0 || j < i {
		t.Fatalf("install.ps1 has no %s between markers", name)
	}
	return s[i+len(begin) : j]
}

func powershell(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"pwsh", "powershell"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	if runtime.GOOS == "windows" {
		t.Fatal("no PowerShell on a Windows runner")
	}
	t.Skip("no PowerShell here; the Windows test job runs this")
	return ""
}

// The installer's restart stops only this installation's `aimem serve`
// processes, by executable path: the binary and its parked copies. A second
// installation's service under the same OS user, run from another
// directory, is never selected, and neither is any other aimem command.
func TestRestartSelectsOnlyThisInstallationsService(t *testing.T) {
	ps := powershell(t)
	fn := extract(t, "Select-ServeProcess")
	mine := `C:\Users\op\AppData\Local\aimem\bin\aimem.exe`
	script := fn + `
$exe = '` + mine + `'
$procs = @(
  [pscustomobject]@{ ProcessId = 1; ExecutablePath = $exe; CommandLine = '"' + $exe + '" serve' },
  [pscustomobject]@{ ProcessId = 2; ExecutablePath = $exe + '.old-20261005101500'; CommandLine = '"' + $exe + '.old-20261005101500" serve' },
  [pscustomobject]@{ ProcessId = 3; ExecutablePath = 'C:\agents\worker\aimem\bin\aimem.exe'; CommandLine = '"C:\agents\worker\aimem\bin\aimem.exe" serve' },
  [pscustomobject]@{ ProcessId = 4; ExecutablePath = $exe; CommandLine = '"' + $exe + '" mcp' },
  [pscustomobject]@{ ProcessId = 5; ExecutablePath = $null; CommandLine = 'aimem.exe serve' },
  [pscustomobject]@{ ProcessId = 6; ExecutablePath = $exe.ToUpper(); CommandLine = '"' + $exe + '" serve' },
  [pscustomobject]@{ ProcessId = 7; ExecutablePath = $exe + 'x'; CommandLine = '"' + $exe + 'x" serve' },
  [pscustomobject]@{ ProcessId = 8; ExecutablePath = $exe; CommandLine = '"' + $exe + '" observe' }
)
ConvertTo-Json -Compress @(Select-ServeProcess $exe $procs | ForEach-Object { $_.ProcessId })
`
	// -File, not -Command -: PowerShell reads standard input line by line
	// and drops a multi-line function definition.
	file := filepath.Join(t.TempDir(), "select.ps1")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", file).CombinedOutput()
	if err != nil {
		t.Fatalf("powershell: %v\n%s", err, out)
	}
	var ids []int
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &ids); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	slices.Sort(ids)
	if want := []int{1, 2, 6}; !slices.Equal(ids, want) {
		t.Fatalf("selected %v, want %v: this binary and its parked copy (any case), never another installation, another command or an unreadable path", ids, want)
	}
}
