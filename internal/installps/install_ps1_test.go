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
	fn := extract(t, "Get-ComparablePath") + extract(t, "Select-ServeProcess")
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
	fn := extract(t, "Get-ComparablePath") + extract(t, "Select-SyncProcess")
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

// runSyncStop runs Stop-AimemSync with no wait against a constructed
// listing: Get-SyncCandidates returns $global:procs, and Stop-SyncByHandle
// records the PID it is asked to end in $global:ended and drops it from the
// listing unless the PID is in $global:stuck. Test-ProcessAlive is the real
// one unless the case replaces it. It prints the ended PIDs in order and
// the thrown message, if any, as THROWN.
func runSyncStop(t *testing.T, cases string) string {
	t.Helper()
	ps := powershell(t)
	script := "function Say($m) { Write-Host \"==> $m\" }\n" + extract(t, "Get-ComparablePath") + extract(t, "Select-SyncProcess") + extract(t, "Stop-AimemSync") + `
$exe = 'C:\Users\op\AppData\Local\aimem\bin\aimem.exe'
$me = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
function P($id, $verb, $owner = $me, $created = [datetime]'2026-11-25T04:00:00') {
  [pscustomobject]@{ ProcessId = $id; ExecutablePath = $exe; CommandLine = '"' + $exe + '" ' + $verb; OwnerSid = $owner; CreationDate = $created }
}
$global:ended = @()
$global:stuck = @()
function Get-SyncCandidates($exe) { @($global:procs) }
function Stop-SyncByHandle($p) {
  $global:ended += $p.ProcessId
  if ($global:stuck -notcontains $p.ProcessId) { $global:procs = @($global:procs | Where-Object ProcessId -ne $p.ProcessId) }
}
` + cases + `
try { Stop-AimemSync $exe 0 } catch { Write-Host "THROWN: $($_.Exception.Message)" }
Write-Host "ENDED: $($global:ended -join ',')"
`
	file := filepath.Join(t.TempDir(), "sync-stop.ps1")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", file).CombinedOutput()
	if err != nil {
		t.Fatalf("powershell: %v\n%s", err, out)
	}
	return strings.ReplaceAll(string(out), "\r\n", "\n")
}

// Every sync is ended, and seen gone, before any leg: the ssh push leg is
// `aimem export-events | ssh ... import-events`, whose status is ssh's, so a
// local export ended under a living sync lets that sync write its push
// cursor past events it never sent. The listing puts the leg first.
func TestSyncStopEndsTheSyncBeforeItsLegs(t *testing.T) {
	out := runSyncStop(t, `
function Test-ProcessAlive($p) { $true }
$global:procs = @((P 2 'export-events --since 01a2'), (P 1 'sync --all-hubs'), (P 3 'import-events'))
`)
	if strings.Contains(out, "THROWN:") || !strings.Contains(out, "ENDED: 1,2,3\n") {
		t.Fatalf("want the sync ended first, then its legs (ENDED: 1,2,3):\n%s", out)
	}
}

// A leg the sync started after the first listing, before it was ended, is
// found by the listing taken once every sync is gone, and ended too.
func TestSyncStopEndsALegStartedAfterTheListing(t *testing.T) {
	out := runSyncStop(t, `
function Test-ProcessAlive($p) { $true }
$global:procs = @((P 2 'export-events --since 01a2'), (P 1 'sync --all-hubs'))
$global:late = P 4 'import-events'
function Stop-SyncByHandle($p) {
  $global:ended += $p.ProcessId
  if ($p.ProcessId -eq 1) { $global:procs = @($global:procs | Where-Object ProcessId -ne 1) + $global:late }
  else { $global:procs = @($global:procs | Where-Object ProcessId -ne $p.ProcessId) }
}
`)
	if strings.Contains(out, "THROWN:") || !strings.Contains(out, "ENDED: 1,2,4\n") {
		t.Fatalf("want the late leg ended as well (ENDED: 1,2,4):\n%s", out)
	}
}

// A sync that cannot be ended leaves every leg running, and the upgrade
// stops before the swap.
func TestSyncStopEndsNoLegWhenTheSyncCannotBeEnded(t *testing.T) {
	out := runSyncStop(t, `
function Test-ProcessAlive($p) { $true }
$global:procs = @((P 2 'export-events --since 01a2'), (P 1 'sync --all-hubs'))
$global:stuck = @(1)
`)
	if !strings.Contains(out, "THROWN: could not end aimem sync (PID 1), so no sync leg was ended") || !strings.Contains(out, "ENDED: 1\n") {
		t.Fatalf("want only the sync tried and a refusal naming it:\n%s", out)
	}
}

// An owner that cannot be read refuses the upgrade only for a process that
// still runs: an owner read also fails for a process that ended after it
// was listed. The real Test-ProcessAlive is used, on this PowerShell's own
// process (still running) and on its PID with another creation time (gone).
func TestSyncStopRefusesAnUnreadableOwnerOnlyWhileItRuns(t *testing.T) {
	gone := runSyncStop(t, `
$global:procs = @((P $PID 'sync --all-hubs' $null))
`)
	if strings.Contains(gone, "THROWN:") || !strings.Contains(gone, "ENDED: \n") {
		t.Errorf("want a gone process with an unreadable owner neither refused nor ended:\n%s", gone)
	}
	alive := runSyncStop(t, `
$self = Get-CimInstance Win32_Process -Filter "ProcessId = $PID"
$global:procs = @((P $PID 'sync --all-hubs' $null $self.CreationDate))
`)
	if !strings.Contains(alive, "and its owner cannot be read") || !strings.Contains(alive, "ENDED: \n") {
		t.Errorf("want a running process with an unreadable owner refused and not ended:\n%s", alive)
	}
}

// Both selectors compare an installation path and a process path in one
// form: a process keeps the form of the path it was started with, so an
// 8.3 short path on one side and the long path on the other name the same
// binary and must match. The directory has a long name so that Windows
// gives it a short one; a volume that makes no 8.3 names skips the test.
func TestSelectorsMatchShortAndLongForms(t *testing.T) {
	ps := powershell(t)
	dir := filepath.Join(t.TempDir(), "aimem-longnamed-installation")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	long := filepath.Join(dir, "aimem.exe")
	if err := os.WriteFile(long, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	script := extract(t, "Get-ComparablePath") + extract(t, "Select-ServeProcess") + extract(t, "Select-SyncProcess") + `
$long = '` + long + `'
$short = (New-Object -ComObject Scripting.FileSystemObject).GetFile($long).ShortPath
if ($short -eq $long -or $short -notmatch '~') { Write-Host 'NO83'; exit 0 }
$me = 'S-1-5-21-1-2-3-1001'
function P($id, $path, $verb) {
  [pscustomobject]@{ ProcessId = $id; ExecutablePath = $path; CommandLine = '"' + $path + '" ' + $verb; OwnerSid = $me }
}
$ids = @()
$ids += @(Select-ServeProcess $long @((P 1 $short 'serve')) | ForEach-Object { $_.ProcessId })
$ids += @(Select-ServeProcess $short @((P 2 $long 'serve')) | ForEach-Object { $_.ProcessId })
$ids += @(Select-SyncProcess $long $me @((P 3 $short 'sync --all-hubs')) | ForEach-Object { $_.ProcessId })
$ids += @(Select-SyncProcess $short $me @((P 4 $long 'sync --all-hubs')) | ForEach-Object { $_.ProcessId })
Write-Host "SHORT $short"
Write-Host "IDS $($ids -join ',')"
`
	file := filepath.Join(t.TempDir(), "forms.ps1")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", file).CombinedOutput()
	if err != nil {
		t.Fatalf("powershell: %v\n%s", err, raw)
	}
	out := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if strings.Contains(out, "NO83") {
		t.Skip("this volume makes no 8.3 short names")
	}
	if !strings.Contains(out, "IDS 1,2,3,4\n") {
		t.Fatalf("want both selectors to match a short and a long form of one path, each way round (IDS 1,2,3,4):\n%s", out)
	}
}
