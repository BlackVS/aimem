package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// runPowerShellUpgrade runs install.ps1's Invoke-Upgrade against the
// sandbox: the old release serves the state root (a real `aimem serve`,
// started and stopped by its own process id in place of the logon task),
// a marker records the state before the upgrade, and the new binary goes
// in. A rollback ends in a thrown message, printed as THROWN.
func runPowerShellUpgrade(t *testing.T, s *sandbox, newBinary, wait string) string {
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
` + extract(t, "install.ps1", "Invoke-Upgrade") + `
$Exe = Join-Path $env:SANDBOX 'bin\aimem.exe'
$global:svc = $null
$global:starts = 0
function Stop-AimemService {
  if ($global:svc) {
    Stop-Process -Id $global:svc.Id -Force -ErrorAction SilentlyContinue
    $global:svc.WaitForExit(10000) | Out-Null
    $global:svc = $null
  }
}
function Start-AimemService {
  $global:starts++
  $log = Join-Path $env:SANDBOX "serve-$($global:starts)"
  $global:svc = Start-Process -FilePath $Exe -ArgumentList 'serve' -PassThru -NoNewWindow -RedirectStandardOutput "$log.out" -RedirectStandardError "$log.err"
}
try {
  Start-AimemService
  if (-not (Wait-AimemHealth $Exe 'v0.0.1' 20)) { Write-Host 'SETUP: the old release did not come up'; exit 99 }
  [IO.File]::WriteAllText((Join-Path $env:AIMEM_STATE_DIR 'marker'), 'before' + [char]10)
  try { Invoke-Upgrade $Exe $env:NEW_BINARY } catch { Write-Host "THROWN: $($_.Exception.Message)" }
} finally {
  Stop-AimemService
}
`
	file := filepath.Join(s.dir, "upgrade.ps1")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", file)
	cmd.Env = append(s.env, "SANDBOX="+s.dir, "NEW_BINARY="+newBinary, "AIMEM_UPGRADE_WAIT="+wait)
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
	s := newSandbox(t, binary(t, "v0.0.1"))
	out := runPowerShellUpgrade(t, s, binary(t, "v0.0.2"), "20")
	checkSuccess(t, s, out)
}

func TestPowerShellUpgradeRollsBackBinaryAndState(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("install.ps1 runs on Windows only; the Windows test job runs this")
	}
	s := newSandbox(t, binary(t, "v0.0.1"))
	out := runPowerShellUpgrade(t, s, binary(t, "broken"), "3")
	checkRolledBack(t, s, out)
}
