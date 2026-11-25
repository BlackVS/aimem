package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// runPowerShellUpgrade runs install.ps1's Invoke-Upgrade against the
// sandbox: the old release serves the state root (a real `aimem serve`,
// started and stopped by its own process id in place of the logon task),
// or in an offline sandbox nothing serves it, a marker records the state
// before the upgrade, and the new binary goes in. A rollback ends in a thrown message, printed as THROWN.
// The stand-in stop ends this installation's sync processes as the
// installer's does (Stop-AimemSync, with a 2 s wait).
//
// With stuck given (the path of the stuck stand-in), three processes that
// do not end by themselves run before the upgrade, each held by its handle:
// "sync", a sync of this installation's parked binary; "other", a sync of
// another installation; and "mcp", this installation's MCP server. Every
// start of the service prints which of them are still alive.
func runPowerShellUpgrade(t *testing.T, s *sandbox, newBinary, wait string, stuck ...string) string {
	return runPowerShellUpgradeRefusing(t, s, newBinary, wait, 0, stuck...)
}

// runPowerShellUpgradeRefusing is runPowerShellUpgrade whose service stop
// number refuse (1 before the swap, 2 in the rollback; 0 for none) has its
// sync part throw, as the sync stop does when it cannot identify or end a
// process of this installation.
func runPowerShellUpgradeRefusing(t *testing.T, s *sandbox, newBinary, wait string, refuse int, stuck ...string) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("install.ps1 runs on Windows only; the Windows test job runs this")
	}
	ps := ""
	for _, name := range []string{"powershell", "pwsh"} {
		if p, err := exec.LookPath(name); err == nil {
			ps = p
			break
		}
	}
	if ps == "" {
		t.Fatal("no PowerShell on a Windows runner")
	}
	script := `$ErrorActionPreference = 'Stop'
function Say($m) { Write-Host "==> $m" }
` + extract(t, "install.ps1", "Invoke-Upgrade") + extract(t, "install.ps1", "Get-ComparablePath") +
		extract(t, "install.ps1", "Select-SyncProcess") +
		extract(t, "install.ps1", "Stop-AimemSync") + `
$Exe = Join-Path $env:SANDBOX 'bin\aimem.exe'
$global:svc = $null
$global:starts = 0
$global:stuck = [ordered]@{}
function Start-Stuck($name, $path, $verb) {
  New-Item -ItemType Directory -Force (Split-Path -Parent $path) | Out-Null
  Copy-Item $env:STUCK $path
  $si = New-Object Diagnostics.ProcessStartInfo $path, $verb
  $si.UseShellExecute = $false
  $si.CreateNoWindow = $true
  $global:stuck[$name] = [Diagnostics.Process]::Start($si)
}
function Stop-Stuck {
  foreach ($p in $global:stuck.Values) {
    if (-not $p.HasExited) { try { $p.Kill() } catch [InvalidOperationException] {} }
    $p.WaitForExit(10000) | Out-Null
    $p.Dispose()
  }
}
# The stand-in service is started through Process.Start, which holds the
# process handle from creation, and is stopped and waited on through that
# handle. Windows PowerShell 5.1's Start-Process -PassThru keeps only the
# PID: a stand-in that exits at once (the broken release) leaves its PID
# free for the whole health wait, and under load the wait then opened
# another process by that PID ("Access is denied"), and the stop could
# have ended an unrelated one.
$global:stops = 0
function Stop-StandInServe {
  $p = $global:svc
  if (-not $p) { return }
  $global:svc = $null
  if (-not $p.HasExited) { try { $p.Kill() } catch [InvalidOperationException] {} }
  if (-not $p.WaitForExit(10000)) { throw "the stand-in service $($p.Id) did not exit" }
  [IO.File]::WriteAllText("$($p.Log).out", $p.Out.Result)
  [IO.File]::WriteAllText("$($p.Log).err", $p.Err.Result)
  $p.Dispose()
}
# The installer's shape: the sync stop, then the serve stop whatever the
# sync stop did. STOP_REFUSES names the stop (1 before the swap, 2 in the
# rollback) whose sync part throws.
function Stop-AimemService {
  $global:stops++
  $n = $global:stops
  Invoke-AimemStop {
    if ($env:STOP_REFUSES -eq "$n") { throw 'a stand-in refusal' }
    Stop-AimemSync $Exe 2
  } {
    Stop-StandInServe
    if ($env:STOP_REFUSES) { Write-Host "SERVE STOPPED $n" }
  }
}
function Start-AimemService {
  $global:starts++
  if ($env:STOP_REFUSES) { Write-Host "STARTED $($global:starts)" }
  # Starting a running service is a no-op, as Start-ScheduledTask is for a
  # running task; a second stand-in would orphan the first one's handle.
  if ($global:svc -and -not $global:svc.HasExited) { return }
  if ($global:stuck.Count) {
    Write-Host "START $($global:starts): $(($global:stuck.Keys | ForEach-Object { "$_=$(-not $global:stuck[$_].HasExited)" }) -join ' ')"
  }
  $si = New-Object Diagnostics.ProcessStartInfo $Exe, 'serve'
  $si.UseShellExecute = $false
  $si.CreateNoWindow = $true
  $si.WorkingDirectory = (Get-Location).Path
  $si.RedirectStandardOutput = $true
  $si.RedirectStandardError = $true
  $p = [Diagnostics.Process]::Start($si)
  # Drain both pipes on the thread pool, so a full pipe never stalls it.
  $p | Add-Member Out $p.StandardOutput.ReadToEndAsync()
  $p | Add-Member Err $p.StandardError.ReadToEndAsync()
  $p | Add-Member Log (Join-Path $env:SANDBOX "serve-$($global:starts)")
  $global:svc = $p
}
try {
  if ($env:STUCK) {
    Start-Stuck 'sync' "$Exe.old-20000101T000000Z" 'sync'
    Start-Stuck 'other' (Join-Path $env:SANDBOX 'other\aimem.exe') 'sync'
    Start-Stuck 'mcp' "$Exe.old-20000101T000001Z" 'mcp'
  }
  if ($env:OFFLINE -eq '1') {
    New-Item -ItemType Directory -Force $env:STATE | Out-Null
  } else {
    Start-AimemService
    if (-not (Wait-AimemHealth $Exe 'v0.0.1' 20)) { Write-Host 'SETUP: the old release did not come up'; exit 99 }
  }
  [IO.File]::WriteAllText((Join-Path $env:STATE 'marker'), 'before' + [char]10)
  try { Invoke-Upgrade $Exe $env:NEW_BINARY } catch { Write-Host "THROWN: $($_.Exception.Message)" }
} finally {
  Stop-AimemService
  Stop-Stuck
}
`
	file := filepath.Join(s.dir, "upgrade.ps1")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", file)
	// The sandbox USERPROFILE has no local application data folder, so
	// PowerShell would write its module analysis cache relative to the
	// working directory, which is this package in the tree. Name the
	// cache and the working directory inside the sandbox instead.
	cmd.Dir = s.dir
	cmd.Env = append(s.env, "SANDBOX="+s.dir, "NEW_BINARY="+newBinary, "AIMEM_UPGRADE_WAIT="+wait,
		"PSModuleAnalysisCachePath="+filepath.Join(s.dir, "ps-module-analysis-cache"))
	if len(stuck) > 0 {
		cmd.Env = append(cmd.Env, "STUCK="+stuck[0])
	}
	if refuse > 0 {
		cmd.Env = append(cmd.Env, "STOP_REFUSES="+strconv.Itoa(refuse))
	}
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 99 {
		t.Fatalf("setup failed:\n%s", out)
	} else if err != nil {
		t.Fatalf("powershell: %v\n%s", err, out)
	}
	return string(out)
}

func TestPowerShellUpgradeKeepsBackupAndReportsBothVersions(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("install.ps1 runs on Windows only; the Windows test job runs this")
	}
	for _, offline := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "stopped"}[offline], func(t *testing.T) {
			s := newSandbox(t, binary(t, "v0.0.1"), offline)
			checkSuccess(t, s, runPowerShellUpgrade(t, s, binary(t, "v0.0.2"), "20"))
		})
	}
}

func TestPowerShellUpgradeRollsBackBinaryAndState(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("install.ps1 runs on Windows only; the Windows test job runs this")
	}
	for _, offline := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "stopped"}[offline], func(t *testing.T) {
			s := newSandbox(t, binary(t, "v0.0.1"), offline)
			checkRolledBack(t, s, runPowerShellUpgrade(t, s, binary(t, "broken"), "15"))
		})
	}
}

// A healthy upgrade keeps the newest two state copies of each kind and
// removes the older ones; a rolled-back one removes nothing, and its report
// is a plain line before the thrown one.
func TestPowerShellUpgradeKeepsTheNewestTwoStateCopies(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("install.ps1 runs on Windows only; the Windows test job runs this")
	}
	s := newSandbox(t, binary(t, "v0.0.1"), false)
	c := seedStateCopies(t, s)
	out := runPowerShellUpgrade(t, s, binary(t, "v0.0.2"), "20")
	if strings.Contains(out, "THROWN:") {
		t.Fatalf("the upgrade failed:\n%s", out)
	}
	c.checkPruned(t, s, out)
}

func TestPowerShellUpgradeThatRollsBackRemovesNoStateCopy(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("install.ps1 runs on Windows only; the Windows test job runs this")
	}
	s := newSandbox(t, binary(t, "v0.0.1"), false)
	c := seedStateCopies(t, s)
	out := runPowerShellUpgrade(t, s, binary(t, "broken"), "15")
	plain := false
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "ROLLED BACK: aimem v0.0.1 is running again") {
			plain = true
		}
	}
	if !plain || !strings.Contains(out, "THROWN: ROLLED BACK:") {
		t.Errorf("want the rollback report as a plain line and as the thrown message:\n%s", out)
	}
	c.checkNothingRemoved(t, out)
}

// The upgrade ends a sync of this installation that is still running, and
// only that: by the first start after the swap the parked binary's sync is
// gone, while another installation's sync and this installation's MCP
// server run on (the test ends them through its own handles). Before, the
// sync task's stop ended only its console wrapper, and the old sync ran on
// beside the new binary.
func TestPowerShellUpgradeEndsThisInstallationsSyncBeforeTheSwap(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("install.ps1 runs on Windows only; the Windows test job runs this")
	}
	s := newSandbox(t, binary(t, "v0.0.1"), true)
	out := runPowerShellUpgrade(t, s, binary(t, "v0.0.2"), "20", binary(t, "stuck"))
	if strings.Contains(out, "THROWN:") {
		t.Fatalf("the upgrade failed:\n%s", out)
	}
	if !strings.Contains(out, "START 1: sync=False other=True mcp=True") {
		t.Errorf("want only this installation's sync ended before the new binary starts:\n%s", out)
	}
	if !strings.Contains(out, "ending aimem sync (PID ") {
		t.Errorf("want the upgrade to name the sync it ended:\n%s", out)
	}
	checkSuccess(t, s, out)
}

// A stop that throws before the swap (the sync stop cannot identify one of
// this installation's processes, or cannot end it) leaves the binary and
// the state root as they were: the old binary stays, no state copy is
// made, the serve stop still runs, and the service is started again.
func TestPowerShellUpgradeThatCannotStopChangesNothing(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("install.ps1 runs on Windows only; the Windows test job runs this")
	}
	s := newSandbox(t, binary(t, "v0.0.1"), false)
	out := runPowerShellUpgradeRefusing(t, s, binary(t, "v0.0.2"), "20", 1)
	if !strings.Contains(out, "THROWN: the service could not be stopped for the upgrade; the binary and the state root are unchanged: a stand-in refusal") {
		t.Errorf("want the refusal reported with the binary and state unchanged:\n%s", out)
	}
	if !strings.Contains(out, "SERVE STOPPED 1") {
		t.Errorf("want the serve stop run although the sync stop threw:\n%s", out)
	}
	if got := s.version(t); got != "aimem v0.0.1" {
		t.Errorf("installed binary reports %q; want the old aimem v0.0.1 kept\n%s", got, out)
	}
	if b := s.siblings(t, ".backup-"); len(b) != 0 {
		t.Errorf("want no state copy, found %v\n%s", b, out)
	}
	if !strings.Contains(out, "STARTED 2") {
		t.Errorf("want the service started again after the refusal:\n%s", out)
	}
}

// In the rollback, a sync stop that throws (a sync the installer cannot end
// or identify started while the sync task was enabled again for the health
// wait) is reported, and the serve stop still runs before the binary and
// the state root are put back.
func TestPowerShellRollbackStopsServeWhenTheSyncStopRefuses(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("install.ps1 runs on Windows only; the Windows test job runs this")
	}
	s := newSandbox(t, binary(t, "v0.0.1"), false)
	out := runPowerShellUpgradeRefusing(t, s, binary(t, "broken"), "15", 2)
	if !strings.Contains(out, "rolling back while the stop could not finish: a stand-in refusal") {
		t.Errorf("want the rollback to report the refused stop:\n%s", out)
	}
	i, j := strings.Index(out, "SERVE STOPPED 2"), strings.Index(out, "STARTED 3")
	if i < 0 || j < i {
		t.Errorf("want the serve stopped in the rollback, before the old release starts again:\n%s", out)
	}
	checkRolledBack(t, s, out)
}
