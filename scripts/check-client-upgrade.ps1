# A real client upgrade on a hosted Windows runner, from the previous
# release to the tree under test: the checks of scripts/check-client-upgrade.sh
# (read its header), without the team join, whose path on Windows is the
# same shell script's. The service is the aimem-serve logon task.
#
# scripts/upgrade-versions.sh derives FROM and TO (Git for Windows' bash
# runs it). README's boot.ps1 block installs FROM; FROM's own
# `aimem upgrade --release TO` installs an export of the tree. Before the
# upgrade FROM's binary serves a throwaway hub and FROM wires a checkout to
# it; after the upgrade the tree's MCP server lists the standalone set there.
# The failure run's stand-in binary is compiled here with Add-Type.
#
# It installs into the account it runs in, so it runs only on a CI runner,
# and it needs go to build the tree.
$ErrorActionPreference = 'Stop'
if ($env:CI -ne 'true' -or $env:OS -ne 'Windows_NT') {
  [Console]::Error.WriteLine('check-client-upgrade: runs only on a Windows CI runner (it installs into this account)')
  exit 2
}
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root
$versions = & bash scripts/upgrade-versions.sh
if ($LASTEXITCODE -ne 0) { throw 'FAIL: scripts/upgrade-versions.sh could not derive the versions' }
$cfg = @{}
foreach ($l in @($versions) + @(Get-Content (Join-Path $root 'scripts\client-upgrade.env'))) {
  if ($l -match '^(FROM|TO|ROLLBACK_LINE_SINCE)=(.*)$') { $cfg[$Matches[1]] = $Matches[2] }
}
$From = $cfg['FROM']; $To = $cfg['TO']; $Since = $cfg['ROLLBACK_LINE_SINCE']
foreach ($v in $From, $To, $Since) {
  if ($v -notmatch '^v[0-9]+\.[0-9]+\.[0-9]+$') { throw "FAIL: bad version '$v'" }
}
"upgrade: $From (the previous release) -> $To (this tree, $(git rev-parse --short HEAD))"
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
$f = Join-Path $W "SHA256SUMS.$From"
Invoke-WebRequest "$DL/$From/SHA256SUMS" -OutFile $f -UseBasicParsing
$SumsFrom = @{}
foreach ($l in Get-Content $f) { $p = $l -split '\s+'; if ($p.Count -ge 2) { $SumsFrom[$p[1].TrimStart('*')] = $p[0].ToLower() } }

# Runs an executable with its output in files; returns @{Out; Err; Code}.
# Start-Process, not &: PowerShell 5.1 would wrap the command's stderr
# lines in error records. Vars names environment variables set for this
# command only; In is a file for its standard input; Dir its working
# directory. Start-Process joins the arguments with spaces and quotes none,
# so an argument with whitespace is quoted here.
function Invoke-Exe($exe, [string[]]$a, $Vars = @{}, $In = $null, $Dir = $W) {
  $log = Join-Path $W ('run-' + [guid]::NewGuid().ToString('N').Substring(0, 8))
  $q = @($a | ForEach-Object { if ($_ -match '\s') { '"' + $_ + '"' } else { $_ } })
  $saved = @{}
  foreach ($k in $Vars.Keys) { $saved[$k] = [Environment]::GetEnvironmentVariable($k); [Environment]::SetEnvironmentVariable($k, $Vars[$k]) }
  try {
    $sp = @{ FilePath = $exe; ArgumentList = $q; NoNewWindow = $true; PassThru = $true; WorkingDirectory = $Dir
      RedirectStandardOutput = "$log.out"; RedirectStandardError = "$log.err" }
    if ($In) { $sp.RedirectStandardInput = $In }
    $p = Start-Process @sp
    $null = $p.Handle # keeps the exit code readable after the exit
    $p.WaitForExit()
  } finally {
    foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k]) }
  }
  @{ Out = [IO.File]::ReadAllText("$log.out"); Err = [IO.File]::ReadAllText("$log.err"); Code = $p.ExitCode }
}
# Runs the installed aimem with its output in a log, shown; returns its exit code.
function Invoke-Logged($log, [string[]]$a, $Vars = @{}) {
  $r = Invoke-Exe $Bin $a -Vars $Vars
  Set-Content $log ($r.Out + $r.Err)
  Get-Content $log | Out-Host
  $r.Code
}
function Assert-Serving($v, $when) {
  $h = Invoke-Exe $Bin @('health')
  if (($h.Out + $h.Err) -notlike "*`"version`":`"$v`"*") { $h.Out + $h.Err; Fail "no health at $v ($when)" }
  $task = Get-ScheduledTask -TaskName 'aimem-serve' -ErrorAction SilentlyContinue
  if (-not $task -or $task.State -ne 'Running') { Fail "the aimem-serve logon task is not running ($when)" }
  "ok: serving $v ($when)"
}
# Builds an aiforge export of release $To in $dir, with the tree's
# installers and plugin and $exe as the platform's asset.
function Export-Tree($dir, $exe) {
  $x = Join-Path $dir "aimem\$To"; New-Item -ItemType Directory -Path $x -Force | Out-Null
  Copy-Item (Join-Path $root 'install.sh'), (Join-Path $root 'install.ps1') $x
  Copy-Item (Join-Path $root '.opencode\plugin\aimem.ts') (Join-Path $x 'aimem-opencode.ts')
  Copy-Item $exe (Join-Path $x $Asset)
  $lines = foreach ($n in 'install.sh', 'install.ps1', 'aimem-opencode.ts', $Asset) { "$(Sha (Join-Path $x $n))  $n" }
  [IO.File]::WriteAllText((Join-Path $x 'SHA256SUMS'), (($lines -join "`n") + "`n"))
  [IO.File]::WriteAllText((Join-Path $dir 'MANIFEST'), "aiforge-export 1`naimem $To`n")
}

"--- the tree, built as $To"
$Tree = Join-Path $W 'tree\aimem.exe'
$r = Invoke-Exe 'go' @('build', '-trimpath', '-ldflags', "-s -w -X main.version=$To", '-o', $Tree, './cmd/aimem') -Vars @{ CGO_ENABLED = '0' } -Dir $root
if ($r.Code -ne 0) { $r.Err; Fail 'the tree does not build' }
if ((Invoke-Exe $Tree @('version')).Out.Trim() -ne "aimem $To") { Fail "the tree's build does not report $To" }
Export-Tree (Join-Path $W 'export') $Tree
$Stub = Join-Path $W 'stand-in.exe'
Add-Type -OutputType ConsoleApplication -OutputAssembly $Stub -TypeDefinition @"
public static class Stub {
  public static int Main(string[] a) {
    if (a.Length > 0 && a[0] == "version") { System.Console.WriteLine("aimem $To"); return 0; }
    return 1;
  }
}
"@
Export-Tree (Join-Path $W 'fail-export') $Stub

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
if ((Sha $Bin) -ne $SumsFrom[$Asset]) { Fail "the installed binary is not $From's $Asset" }

"--- a throwaway hub on $From's binary; a checkout wired by $From"
$H = Join-Path $W 'hub'
foreach ($s in 'home', 'state', 'tls') { New-Item -ItemType Directory -Path (Join-Path $H $s) -Force | Out-Null }
$HubExe = Join-Path $H 'aimem.exe'
Copy-Item $Bin $HubExe
$r = Invoke-Exe 'go' @('run', (Join-Path (& go env GOROOT) 'src\crypto\tls\generate_cert.go'), '--host', '127.0.0.1', '--ca', '--duration', '2h') -Dir (Join-Path $H 'tls')
if ($r.Code -ne 0) { $r.Err; Fail 'no certificate for the throwaway hub' }
$Port = 47941
$HubEnv = @{ AIMEM_STATE_DIR = (Join-Path $H 'state'); AIMEM_SOCKET = (Join-Path $H 'state\aimem.sock')
  HOME = (Join-Path $H 'home'); USERPROFILE = (Join-Path $H 'home') }
$ServeEnv = $HubEnv.Clone()
$ServeEnv.AIMEM_HTTP_LISTEN = "127.0.0.1:$Port"
$ServeEnv.AIMEM_HTTP_TOKEN = [guid]::NewGuid().ToString('N')
$ServeEnv.AIMEM_TLS_CERT = Join-Path $H 'tls\cert.pem'
$ServeEnv.AIMEM_TLS_KEY = Join-Path $H 'tls\key.pem'
$saved = @{}
foreach ($k in $ServeEnv.Keys) { $saved[$k] = [Environment]::GetEnvironmentVariable($k); [Environment]::SetEnvironmentVariable($k, $ServeEnv[$k]) }
try {
  $HubProc = Start-Process -FilePath $HubExe -ArgumentList 'serve' -NoNewWindow -PassThru `
    -RedirectStandardOutput (Join-Path $H 'serve.out') -RedirectStandardError (Join-Path $H 'serve.err')
} finally {
  foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k]) }
}
try {
  function Hub([string[]]$a) {
    $r = Invoke-Exe $HubExe $a -Vars $HubEnv
    if ($r.Code -ne 0) { $r.Err; Fail "aimem $($a[0..1] -join ' ') on the throwaway hub exited $($r.Code)" }
    $r
  }
  for ($i = 0; $i -lt 50; $i++) { if ((Invoke-Exe $HubExe @('health') -Vars $HubEnv).Code -eq 0) { break }; Start-Sleep -Milliseconds 200 }
  $null = Hub @('health')
  $Expiry = [DateTime]::UtcNow.AddDays(30).ToString('yyyy-MM-ddTHH:mm:ssZ')
  $null = Hub @('tasks', 'on', '-p', 'upgrade-scenario')
  # The secret is the one line of hex; it goes to a file and is never printed.
  $tok = @((Hub @('token', 'add', 'upgrade-workstation')).Out -split "`r?`n" | Where-Object { $_ -match '^[0-9a-f]{32,}$' })
  if ($tok.Count -ne 1) { Fail 'aimem token add did not print one token line' }
  $WsToken = Join-Path $H 'workstation.token'
  [IO.File]::WriteAllText($WsToken, $tok[0] + "`n")
  $me = [Security.Principal.WindowsIdentity]::GetCurrent().Name
  & icacls $WsToken /inheritance:r /grant:r "${me}:F" | Out-Null
  $WorkerToken = Join-Path $H 'worker.token'
  $null = Hub @('teams', 'provision', 'create', 'upgrade-scenario', '--team', 'Pilot', '--coordinator', 'upgrade-member',
    '--create-user', '--label', 'upgrade-member', '--expiry', $Expiry, '--output', $WorkerToken)
  $r = Invoke-Exe $Bin @('hub', 'add', 'upgrade', "https://127.0.0.1:$Port", '--token-file', $WsToken, '--ca-file', (Join-Path $H 'tls\cert.pem'))
  if ($r.Code -ne 0) { $r.Err; Fail "$From's hub add exited $($r.Code)" }
  $Co = Join-Path $W 'alone'; New-Item -ItemType Directory -Path $Co | Out-Null
  & git -C $Co init -q
  $r = Invoke-Exe $Bin @('wire', '--project', 'upgrade-scenario', '--hub', 'upgrade') -Dir $Co
  if ($r.Code -ne 0) { $r.Err; Fail "$From's aimem wire exited $($r.Code)" }
  $r = Invoke-Exe $Bin @('task-token', 'set') -In $WorkerToken -Dir $Co
  if ($r.Code -ne 0) { $r.Err; Fail "$From's task-token set exited $($r.Code)" }

  # The names the checkout's MCP server lists, from a session started now.
  $McpIn = Join-Path $W 'mcp.in'
  [IO.File]::WriteAllText($McpIn, (@(
    '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"upgrade-check","version":"1"}}}',
    '{"jsonrpc":"2.0","id":2,"method":"tools/list"}') -join "`n") + "`n")
  function Get-Tools {
    $r = Invoke-Exe $Bin @('mcp') -In $McpIn -Dir $Co
    foreach ($l in $r.Out -split "`r?`n") {
      if (-not $l.Trim()) { continue }
      $m = $l | ConvertFrom-Json
      if ($m.id -eq 2) { return @($m.result.tools | ForEach-Object { $_.name } | Sort-Object) }
    }
    $r.Err; Fail 'the MCP server answered no tools/list'
  }
  "$From lists $((Get-Tools).Count) tools in the checkout"

  "--- $To with a binary that never answers health: the upgrade must roll back"
  $rc = Invoke-Logged (Join-Path $W 'fail.log') @('upgrade', '--release', $To) -Vars @{ AIMEM_BASE = (Join-Path $W 'fail-export'); AIMEM_UPGRADE_WAIT = '10' }
  if ($rc -eq 0) { Fail 'the upgrade to a binary that never serves exited 0' }
  if (-not (Select-String -LiteralPath (Join-Path $W 'fail.log') -SimpleMatch 'ROLLED BACK')) { Fail 'the failed upgrade did not say ROLLED BACK' }
  if ([version]$From.TrimStart('v') -ge [version]$Since.TrimStart('v')) {
    $last = Get-Content (Join-Path $W 'fail.log') | Where-Object { $_.Trim() } | Select-Object -Last 1
    if (-not "$last".StartsWith("aimem: the upgrade to $To failed and was rolled back: aimem $From is running again")) {
      Fail 'the failed upgrade did not end with the line that says it was rolled back'
    }
  }
  if ((Sha $Bin) -ne $SumsFrom[$Asset]) { Fail "the binary after the rollback is not $From's $Asset" }
  Assert-Serving $From 'after the rollback'

  "--- aimem upgrade --release $To, from the tree's export"
  $rc = Invoke-Logged (Join-Path $W 'upgrade.log') @('upgrade', '--release', $To) -Vars @{ AIMEM_BASE = (Join-Path $W 'export') }
  if ($rc -ne 0) { Fail "the upgrade to $To exited $rc" }
  if (-not (Select-String -LiteralPath (Join-Path $W 'upgrade.log') -SimpleMatch "upgraded aimem $From -> $To; service healthy at $To")) {
    Fail "the upgrade did not report a healthy service at $To"
  }
  if ((Sha $Bin) -ne (Sha $Tree)) { Fail "the installed binary is not the tree's build" }
  if ((Sha $Plugin) -ne (Sha (Join-Path $root '.opencode\plugin\aimem.ts'))) { Fail "the OpenCode plugin is not the tree's" }
  if (-not (Select-String -LiteralPath (Join-Path $env:USERPROFILE '.claude\settings.json') -SimpleMatch 'aimem submit-claude')) {
    Fail 'the Claude Code hooks are gone after the upgrade'
  }
  Assert-Serving $To 'after upgrade'

  "--- the list of a session started after the upgrade"
  $names = Get-Tools
  "$To lists $($names.Count) tools in the checkout"
  $team = @($names | Where-Object { $_ -like 'team_*' })
  if (($team -join ' ') -ne 'team_context team_continue team_list team_setup') {
    Fail "the checkout that never joined lists other team tools than the four a join needs: $($team -join ' ')"
  }
  if (@($names | Where-Object { $_ -like 'task_reservation_*' }).Count -ne 0) { Fail 'the checkout that never joined lists reservation tools' }
  if ($names -notcontains 'update_task') { Fail 'the checkout that never joined does not list the task tools' }
  'ok: the checkout that never joined lists the standalone set'
} finally {
  if ($HubProc) { Stop-Process -Id $HubProc.Id -Force -ErrorAction SilentlyContinue }
}

"PASS: client upgrade $From -> $To (this tree), the rollback, and the list after it"
