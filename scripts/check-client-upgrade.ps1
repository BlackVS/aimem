# A real client upgrade on a hosted Windows runner, from published releases:
# README's boot.ps1 block installs FROM, `aimem upgrade --release TO` moves it
# to TO, and a failed upgrade rolls back. The versions come from
# scripts/client-upgrade.env. The checks are those of
# scripts/check-client-upgrade.sh; the service is the aimem-serve logon task.
#
# The failure run's stand-in binary is compiled here with Add-Type, so the
# check needs no Go toolchain.
#
# It installs into the account it runs in, so it runs only on a CI runner.
$ErrorActionPreference = 'Stop'
if ($env:CI -ne 'true' -or $env:OS -ne 'Windows_NT') {
  [Console]::Error.WriteLine('check-client-upgrade: runs only on a Windows CI runner (it installs into this account)')
  exit 2
}
$root = Split-Path -Parent $PSScriptRoot
$cfg = @{}
foreach ($l in Get-Content (Join-Path $root 'scripts\client-upgrade.env')) {
  if ($l -match '^(FROM|TO)=(.*)$') { $cfg[$Matches[1]] = $Matches[2] }
}
$From = $cfg['FROM']; $To = $cfg['TO']
foreach ($v in $From, $To) {
  if ($v -notmatch '^v[0-9]+\.[0-9]+\.[0-9]+$') { throw "scripts/client-upgrade.env: bad version '$v'" }
}
$Repo = 'BlackVS/aimem'
$DL = "https://github.com/$Repo/releases/download"
$Bin = Join-Path $env:LOCALAPPDATA 'aimem\bin\aimem.exe'
$Asset = 'aimem-windows-amd64.exe'
$Plugin = Join-Path $env:USERPROFILE '.config\opencode\plugins\aimem.ts'
$W = Join-Path $env:RUNNER_TEMP ('client-upgrade-' + [guid]::NewGuid().ToString('N').Substring(0, 8))
New-Item -ItemType Directory -Path $W | Out-Null
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

function Fail($m) { throw "FAIL: $m" }
function Sha($p) { (Get-FileHash -LiteralPath $p -Algorithm SHA256).Hash.ToLower() }
function Get-Sums($tag) {
  $f = Join-Path $W "SHA256SUMS.$tag"
  Invoke-WebRequest "$DL/$tag/SHA256SUMS" -OutFile $f -UseBasicParsing
  $m = @{}
  foreach ($l in Get-Content $f) { $p = $l -split '\s+'; if ($p.Count -ge 2) { $m[$p[1].TrimStart('*')] = $p[0].ToLower() } }
  $m
}
# Runs a native command with its output in a log; returns its exit code.
function Invoke-Logged($log, [string[]]$a) {
  $ErrorActionPreference = 'Continue'
  & $Bin @a *> $log
  $rc = $LASTEXITCODE
  Get-Content $log | Out-Host
  $rc
}
function Assert-Serving($v, $when) {
  $ErrorActionPreference = 'Continue'
  $h = (& $Bin health 2>&1) -join ' '
  if ($h -notlike "*`"version`":`"$v`"*") { $h; Fail "no health at $v ($when)" }
  $task = Get-ScheduledTask -TaskName 'aimem-serve' -ErrorAction SilentlyContinue
  if (-not $task -or $task.State -ne 'Running') { Fail "the aimem-serve logon task is not running ($when)" }
  "ok: serving $v ($when)"
}
$SumsFrom = Get-Sums $From
$SumsTo = Get-Sums $To

"--- one-liner install of $From (README's boot.ps1 block)"
$d = Join-Path $W 'boot'; New-Item -ItemType Directory -Path $d | Out-Null
$f = Join-Path $d 'boot.ps1'
Invoke-WebRequest "$DL/$From/boot.ps1" -OutFile $f -UseBasicParsing
if ((Sha $f) -ne $SumsFrom['boot.ps1']) { Fail "boot.ps1 does not match $From's SHA256SUMS" }
Unblock-File $f
$env:AIMEM_USER_ONLY = '1'
& powershell -NoProfile -ExecutionPolicy RemoteSigned -File $f
$rc = $LASTEXITCODE
Remove-Item Env:AIMEM_USER_ONLY
if ($rc -ne 0) { Fail "the install of $From exited $rc" }
Assert-Serving $From 'after install'

"--- aimem upgrade --release $To"
$rc = Invoke-Logged (Join-Path $W 'upgrade.log') @('upgrade', '--release', $To)
if ($rc -ne 0) { Fail "the upgrade to $To exited $rc" }
if (-not (Select-String -LiteralPath (Join-Path $W 'upgrade.log') -SimpleMatch "upgraded aimem $From -> $To; service healthy at $To")) {
  Fail "the upgrade did not report a healthy service at $To"
}
if ((Sha $Bin) -ne $SumsTo[$Asset]) { Fail "the binary is not $To's $Asset" }
if ((Sha $Plugin) -ne $SumsTo['aimem-opencode.ts']) { Fail "the OpenCode plugin is not $To's aimem-opencode.ts" }
if (-not (Select-String -LiteralPath (Join-Path $env:USERPROFILE '.claude\settings.json') -SimpleMatch 'aimem submit-claude')) {
  Fail 'the Claude Code hooks are gone after the upgrade'
}
Assert-Serving $To 'after upgrade'

"--- back to $From"
$rc = Invoke-Logged (Join-Path $W 'down.log') @('upgrade', '--release', $From)
if ($rc -ne 0) { Fail "the downgrade to $From exited $rc" }
Assert-Serving $From 'after downgrade'

"--- $To with a binary that never answers health: the upgrade must roll back"
$X = Join-Path $W "export\aimem\$To"; New-Item -ItemType Directory -Path $X -Force | Out-Null
foreach ($n in 'install.sh', 'install.ps1', 'aimem-opencode.ts') {
  Invoke-WebRequest "$DL/$To/$n" -OutFile (Join-Path $X $n) -UseBasicParsing
  if ((Sha (Join-Path $X $n)) -ne $SumsTo[$n]) { Fail "$n does not match $To's SHA256SUMS" }
}
Add-Type -OutputType ConsoleApplication -OutputAssembly (Join-Path $X $Asset) -TypeDefinition @"
public static class Stub {
  public static int Main(string[] a) {
    if (a.Length > 0 && a[0] == "version") { System.Console.WriteLine("aimem $To"); return 0; }
    return 1;
  }
}
"@
$lines = foreach ($n in 'install.sh', 'install.ps1', 'aimem-opencode.ts', $Asset) { "$(Sha (Join-Path $X $n))  $n" }
[IO.File]::WriteAllText((Join-Path $X 'SHA256SUMS'), (($lines -join "`n") + "`n"))
[IO.File]::WriteAllText((Join-Path $W 'export\MANIFEST'), "aiforge-export 1`naimem $To`n")
$env:AIMEM_BASE = Join-Path $W 'export'; $env:AIMEM_UPGRADE_WAIT = '10'
$rc = Invoke-Logged (Join-Path $W 'fail.log') @('upgrade', '--release', $To)
Remove-Item Env:AIMEM_BASE, Env:AIMEM_UPGRADE_WAIT
if ($rc -eq 0) { Fail 'the upgrade to a binary that never serves exited 0' }
if (-not (Select-String -LiteralPath (Join-Path $W 'fail.log') -SimpleMatch 'ROLLED BACK')) { Fail 'the failed upgrade did not say ROLLED BACK' }
if ((Sha $Bin) -ne $SumsFrom[$Asset]) { Fail "the binary after the rollback is not $From's $Asset" }
Assert-Serving $From 'after the rollback'

"PASS: client upgrade $From -> $To, and the rollback"
