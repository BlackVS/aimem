package installps

import (
	"encoding/json"
	"maps"
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
	// .gitattributes checks *.ps1 out with CRLF on Windows.
	s := strings.ReplaceAll(string(raw), "\r\n", "\n")
	begin, end := "# BEGIN "+name+"\n", "# END "+name
	i, j := strings.Index(s, begin), strings.Index(s, end)
	if i < 0 || j < i {
		t.Fatalf("install.ps1 has no %s between markers", name)
	}
	return s[i+len(begin) : j]
}

// powershell returns the Windows PowerShell or pwsh to run installer
// pieces with. The installer runs on Windows only, and its functions work
// on Windows paths (pwsh elsewhere resolves C:\ paths as relative), so the
// test runs on Windows, where it is mandatory, and skips elsewhere.
func powershell(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("install.ps1 runs on Windows only; the Windows test job runs this")
	}
	for _, name := range []string{"powershell", "pwsh"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Fatal("no PowerShell on a Windows runner")
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

// The upgrade's sync stop selects only this installation's sync and the
// legs a sync starts, run by the installer's user: the binary or a parked
// copy, with one of the seven resumable verbs as its first argument. Every
// row it must never end is a constructed process that it leaves alone. A
// process of this installation whose owner cannot be read is selected as
// "unknown", which the upgrade never ends and does not swap under.
func TestSyncStopSelectsOnlyThisInstallationsSync(t *testing.T) {
	ps := powershell(t)
	fn := extract(t, "Select-SyncProcess")
	mine := `C:\Users\op\AppData\Local\aimem\bin\aimem.exe`
	script := fn + `
$exe = '` + mine + `'
$me = 'S-1-5-21-1-2-3-1001'
function P($id, $path, $verbs, $owner = $me) {
  $line = if ($path) { '"' + $path + '"' } else { 'aimem.exe' }
  if ($verbs) { $line += ' ' + $verbs }
  [pscustomobject]@{ ProcessId = $id; ExecutablePath = $path; CommandLine = $line; OwnerSid = $owner }
}
$procs = @(
  (P 1 $exe 'sync --all-hubs'),
  (P 2 $exe 'export-events --since 01a2'),
  (P 3 $exe 'import-events'),
  (P 4 $exe 'export-memories'),
  (P 5 $exe 'import-memories'),
  (P 6 $exe 'export-group-config'),
  (P 7 $exe 'import-group-config'),
  (P 8 ($exe + '.old-20261125T040000Z') 'sync --all-hubs'),
  (P 9 $exe.ToUpper() 'sync'),
  (P 10 $exe 'sync' $null),
  ([pscustomobject]@{ ProcessId = 11; ExecutablePath = $exe; CommandLine = $exe + ' import-events'; OwnerSid = $me }),
  (P 20 'C:\agents\worker\aimem\bin\aimem.exe' 'sync --all-hubs'),
  (P 21 $exe 'sync --all-hubs' 'S-1-5-21-1-2-3-1002'),
  (P 22 $exe 'mcp'),
  (P 23 $exe 'serve'),
  (P 24 $exe 'submit-claude'),
  (P 25 $exe 'submit-codex'),
  (P 26 $exe 'submit'),
  (P 27 $exe 'failure-recall'),
  (P 28 $exe 'upgrade --release v0.12.3'),
  (P 29 $exe 'health'),
  (P 30 $exe ''),
  (P 31 $null 'sync --all-hubs'),
  (P 32 $exe 'hub add sync https://hub.example'),
  (P 33 'C:\Tools\aimem.exe' 'sync'),
  (P 34 ($exe + 'x') 'sync'),
  (P 35 $exe 'SYNC'),
  ([pscustomobject]@{ ProcessId = 36; ExecutablePath = 'C:\Program Files\Git\usr\bin\bash.exe'; CommandLine = 'bash -c "ssh hub aimem export-events | ' + $exe + ' import-events"'; OwnerSid = $me }),
  ([pscustomobject]@{ ProcessId = 37; ExecutablePath = 'C:\Windows\System32\OpenSSH\ssh.exe'; CommandLine = 'ssh hub "aimem export-events --since 01a2"'; OwnerSid = $me })
)
$out = [ordered]@{}
Select-SyncProcess $exe $me $procs | ForEach-Object { $out["$($_.ProcessId)"] = $_.Kind }
# A path with a space, given unquoted on the command line, is not split.
$spaced = 'C:\Users\Jo Doe\AppData\Local\aimem\bin\aimem.exe'
$sp = @(
  [pscustomobject]@{ ProcessId = 40; ExecutablePath = $spaced; CommandLine = $spaced + ' sync --all-hubs'; OwnerSid = $me },
  [pscustomobject]@{ ProcessId = 41; ExecutablePath = $spaced; CommandLine = $spaced + ' mcp'; OwnerSid = $me }
)
Select-SyncProcess $spaced $me $sp | ForEach-Object { $out["$($_.ProcessId)"] = $_.Kind }
ConvertTo-Json -Compress $out
`
	file := filepath.Join(t.TempDir(), "select-sync.ps1")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", file).CombinedOutput()
	if err != nil {
		t.Fatalf("powershell: %v\n%s", err, out)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &got); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	want := map[string]string{"1": "stop", "2": "stop", "3": "stop", "4": "stop", "5": "stop", "6": "stop", "7": "stop",
		"8": "stop", "9": "stop", "10": "unknown", "11": "stop", "40": "stop"}
	if !maps.Equal(got, want) {
		t.Fatalf("selected %v,\nwant %v: this installation's binary or parked copy, one of the seven sync verbs first, the installer's user (an unreadable owner as unknown); never another installation, owner, command, an unreadable path, a later argument, a name alone, or bash and ssh", got, want)
	}
}
