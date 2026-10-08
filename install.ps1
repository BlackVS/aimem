# aimem installer for Windows. Idempotent; safe to re-run.
# Usually invoked by boot.ps1 (the one-liner), but can be run directly:
#
#   .\install.ps1 -Target C:\path\to\project      # user install (if needed) + wire project
#   .\install.ps1 -UserOnly                       # user-level install only
#   .\install.ps1 -Bootstrap -Target <dir>        # what boot.ps1 runs; wires
#                                                 # <dir> unless Get-WireSkipReason says not
#   .\install.ps1 -UninstallUser
#
# User install: aimem.exe -> %LOCALAPPDATA%\aimem\bin (added to user PATH),
# Claude Code checkpoint hooks, OpenCode global plugin, Codex checkpoint
# hooks + MCP registration, and a logon scheduled task running
# `aimem serve` (headless via conhost).
# NOTE: Windows support is best-effort; the service uses an AF_UNIX socket
# (supported on Windows 10 1803+ / Go 1.23+ std). Report issues.
param(
  [string]$Target,
  [switch]$UserOnly,
  [switch]$Bootstrap,
  [switch]$UninstallUser
)
$ErrorActionPreference = 'Stop'
$RepoDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$BinDir = Join-Path $env:LOCALAPPDATA 'aimem\bin'
$Exe = Join-Path $BinDir 'aimem.exe'
$ClaudeSettings = Join-Path $env:USERPROFILE '.claude\settings.json'
$OcPluginDir = Join-Path $env:USERPROFILE '.config\opencode\plugins'
$CodexHome = if ($env:AIMEM_CODEX_HOME) { $env:AIMEM_CODEX_HOME } else { Join-Path $env:USERPROFILE '.codex' }
$SubmitCmd = 'aimem submit-claude'
# Codex space-splits hook commands and spawns them directly (no shell),
# so its command must stay a bare program + args.
$CodexSubmitCmd = 'aimem submit-codex'
$SessionStartCmd = 'aimem session-start'

function Say($m) { Write-Host "==> $m" }

# BEGIN Select-ServeProcess
# Select-ServeProcess picks, from Win32_Process objects, the `aimem serve`
# processes of THIS installation: the binary at $exe or one of its parked
# copies ($exe.old-*, which a running service keeps serving after an
# upgrade renamed it). It never selects by name alone: another
# installation's service under the same OS user, run from another
# directory, is left running (docs/DEVELOPMENT.md, "Processes and state in
# tests"). A process whose path cannot be read is not ours to stop.
function Select-ServeProcess($exe, $processes) {
  $full = [IO.Path]::GetFullPath($exe)
  @($processes | Where-Object {
    $path = $_.ExecutablePath
    $path -and ($_.CommandLine -match '\sserve(\s|$)') -and (
      [string]::Equals($path, $full, [StringComparison]::OrdinalIgnoreCase) -or
      $path.StartsWith($full + '.old-', [StringComparison]::OrdinalIgnoreCase))
  })
}
# END Select-ServeProcess

# BEGIN Invoke-Upgrade
# Invoke-Upgrade replaces the installed binary $exe with $new without
# risking the state root. A schema move is one-way, so the state root is
# copied before the new binary first opens it, and a new binary that does
# not come up healthy at its own version is undone: the previous binary
# and the state copy both go back. The caller defines Stop-AimemService
# and Start-AimemService (this installation's service and its sync task,
# nothing else) and Say.
function Invoke-Quiet($exe, [string[]]$argv) {
  # PS 5.1 under EAP=Stop turns a native command's stderr into a
  # terminating error; these probes expect failures, so relax it locally.
  $prevEap = $ErrorActionPreference
  $ErrorActionPreference = 'Continue'
  try { $out = (& $exe @argv 2>$null | Out-String) } catch { $out = '' }
  $ErrorActionPreference = $prevEap
  $out
}
function Get-AimemVersion($exe) {
  "$((Invoke-Quiet $exe @('version')).Trim() -split '\s+' | Select-Object -Index 1)"
}
function Get-AimemHealth($exe) {
  $raw = Invoke-Quiet $exe @('health')
  if (-not $raw.Trim()) { return $null }
  try { $raw | ConvertFrom-Json } catch { $null }
}
function Wait-AimemHealth($exe, $want, $seconds) {
  $deadline = (Get-Date).AddSeconds($seconds)
  do {
    $h = Get-AimemHealth $exe
    if ($h -and $h.version -and (-not $want -or $h.version -eq $want)) { return $true }
    Start-Sleep -Milliseconds 500
  } while ((Get-Date) -lt $deadline)
  $false
}
function Get-StateRoot($exe, $health) {
  # The running service knows its state root; a stopped one's binary
  # resolves it the same way it would (environment, then its env file).
  if ($health -and $health.state_root) { return $health.state_root }
  $named = (Invoke-Quiet $exe @('state-root')).Trim()
  if ($named) { return $named }
  # The order of internal/adapter.StateRoot (Go's home dir on Windows is
  # USERPROFILE), for a binary that cannot answer.
  if ($env:AIMEM_STATE_DIR) { return $env:AIMEM_STATE_DIR }
  $base = if ($env:XDG_STATE_HOME) { $env:XDG_STATE_HOME } else { Join-Path $env:USERPROFILE '.local\state' }
  Join-Path $base 'aimem'
}
function Copy-StateRoot($src, $dst) {
  # robocopy, not Copy-Item: it skips the service socket (an AF_UNIX
  # socket file that Copy-Item and Compress-Archive die on), and
  # /COPY:DATS keeps the owner-only ACLs of the credential files.
  $null = & robocopy $src $dst /E /COPY:DATS /DCOPY:DAT /XF aimem.sock /R:1 /W:1 /NFL /NDL /NJH /NJS /NP
  $LASTEXITCODE -lt 8
}
function Invoke-Upgrade($exe, $new) {
  $wait = if ($env:AIMEM_UPGRADE_WAIT) { [int]$env:AIMEM_UPGRADE_WAIT } else { 30 }
  $oldV = Get-AimemVersion $exe
  $newV = Get-AimemVersion $new
  $root = Get-StateRoot $exe (Get-AimemHealth $exe)
  $ts = (Get-Date).ToUniversalTime().ToString("yyyyMMdd'T'HHmmss'Z'")
  Say "upgrading aimem $oldV -> $newV (state root $root)"
  Stop-AimemService
  $backup = $null
  if (Test-Path $root) {
    $backup = "$root.backup-$ts"
    if (-not (Copy-StateRoot $root $backup)) {
      Remove-Item -Recurse -Force $backup -ErrorAction SilentlyContinue
      Start-AimemService
      throw "could not copy the state root to $backup; nothing was changed"
    }
    Say "state backup: $backup"
  }
  # A running aimem.exe (an `aimem mcp` inside an agent session) blocks
  # Copy-Item over it, but Windows allows RENAMING an in-use exe: park the
  # old file under a unique name and drop the new one in.
  $park = "$exe.old-$ts"
  try { Move-Item $exe $park } catch {
    Start-AimemService
    throw "cannot replace $exe (still locked even for rename): $_"
  }
  Copy-Item $new $exe -Force
  Start-AimemService
  if (Wait-AimemHealth $exe $newV $wait) {
    Say "upgraded aimem $oldV -> $newV; service healthy at $newV"
    if ($backup) { Say "the state backup stays at $backup (remove it once satisfied)" }
    return
  }
  Write-Warning "aimem $newV did not answer health at its version within ${wait}s; rolling back"
  Stop-AimemService
  Move-Item $exe "$exe.failed-$ts" -Force
  Move-Item $park $exe
  if ($backup) {
    try {
      Move-Item $root "$root.failed-$ts"
      if (-not (Copy-StateRoot $backup $root)) { throw 'robocopy failed' }
    } catch {
      throw "the previous binary is back, but the state root could not be restored ($_): copy $backup to $root by hand before starting the service"
    }
  }
  Start-AimemService
  $kept = if ($backup) { " ($backup; the state the failed upgrade left is in $root.failed-$ts)" } else { '' }
  if (Wait-AimemHealth $exe $oldV $wait) {
    throw "ROLLED BACK: aimem $oldV is running again on the state copied before the upgrade$kept"
  }
  throw "ROLLED BACK: the previous binary and state are restored$kept, but the service does not answer health; check it"
}
# END Invoke-Upgrade

# The service hooks Invoke-Upgrade calls: this installation's logon task
# and serve processes, and the sync task, which opens the same databases.
$script:SyncWasEnabled = $false
function Stop-AimemService {
  $sync = Get-ScheduledTask -TaskName 'aimem-sync' -ErrorAction SilentlyContinue
  if ($sync -and $sync.State -ne 'Disabled') {
    $script:SyncWasEnabled = $true
    $sync | Disable-ScheduledTask | Out-Null
  }
  Stop-ScheduledTask 'aimem-sync' -ErrorAction SilentlyContinue
  # Stopping the TASK kills the conhost wrapper but ORPHANS its aimem
  # child, which keeps serving through the socket. Stop this
  # installation's serve processes by executable path, never by name.
  Stop-ScheduledTask 'aimem-serve' -ErrorAction SilentlyContinue
  $candidates = Get-CimInstance Win32_Process -Filter "Name LIKE 'aimem.exe%'"
  Select-ServeProcess $Exe $candidates | ForEach-Object {
    Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue
    Wait-Process -Id $_.ProcessId -Timeout 10 -ErrorAction SilentlyContinue
  }
}
function Start-AimemService {
  Start-ScheduledTask 'aimem-serve'
  if ($script:SyncWasEnabled) { Enable-ScheduledTask -TaskName 'aimem-sync' | Out-Null }
}

# Windows PowerShell 5.1's `-Encoding UTF8` emits a BOM, and Go's
# encoding/json rejects one. A BOM in .aimem.json therefore silently
# voids the project's hub binding and group membership — the file parses
# as absent, and the project falls back to the machine's default hub.
# Every file this script writes goes through these two, BOM-free.
$Utf8NoBom = New-Object System.Text.UTF8Encoding $false
function Write-Text($path, $text) {
  New-Item -ItemType Directory -Force (Split-Path -Parent $path) | Out-Null
  [System.IO.File]::WriteAllText($path, $text, $Utf8NoBom)
}
function Read-Json($path) {
  # -Raw keeps a pre-existing BOM in the string; ConvertFrom-Json in 5.1
  # chokes on it, so strip it before parsing files other tools wrote.
  if (Test-Path $path) { (Get-Content $path -Raw).TrimStart([char]0xFEFF) | ConvertFrom-Json } else { [pscustomobject]@{} }
}
function Write-Json($path, $obj) {
  Write-Text $path (($obj | ConvertTo-Json -Depth 16) + "`r`n")
}

# Merge one checkpoint hook entry, keyed on the "aimem " command marker so
# re-runs find it. Never touches other hooks. Claude Code settings.json and
# Codex hooks.json share the same hooks block shape, so one merger serves
# both clients.
function Add-AgentHook($file, $event, $cmd, $status, $marker) {
  $s = Read-Json $file
  if (-not $s.PSObject.Properties['hooks']) { $s | Add-Member hooks ([pscustomobject]@{}) }
  if (-not $s.hooks.PSObject.Properties[$event]) { $s.hooks | Add-Member $event @() }
  foreach ($entry in $s.hooks.$event) {
    foreach ($h in $entry.hooks) { if ("$($h.command)" -match [regex]::Escape($marker)) { return } }
  }
  $s.hooks.$event = @($s.hooks.$event) + ,([pscustomobject]@{
    hooks = @([pscustomobject]@{ type = 'command'; command = $cmd; timeout = 10; statusMessage = $status })
  })
  Write-Json $file $s
}

# Register-AimemTasks registers (or refreshes) the logon task for
# `aimem serve` and the periodic sync task; neither starts anything.
function Register-AimemTasks {
  # Logon task for `aimem serve`; conhost --headless keeps it windowless.
  Say 'registering logon task aimem-serve'
  $action = New-ScheduledTaskAction -Execute 'conhost.exe' -Argument "--headless `"$Exe`" serve"
  $trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
  $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero)
  Register-ScheduledTask -TaskName 'aimem-serve' -Action $action -Trigger $trigger -Settings $settings -Force | Out-Null
  # Periodic anti-entropy sync (DESIGN-hub-sync): rides the hub API, so
  # Windows machines finally PULL curated knowledge instead of only
  # pushing events. Harmless no-op cadence when no hub is configured.
  Say 'registering sync task aimem-sync (every 10 minutes)'
  $syncAction = New-ScheduledTaskAction -Execute 'conhost.exe' -Argument "--headless `"$Exe`" sync --all-hubs"
  # No -RepetitionDuration: an empty duration repeats indefinitely.
  # [TimeSpan]::MaxValue renders as P99999999DT... which Task Scheduler
  # rejects as out of range (verified live on Windows 11).
  $syncTrigger = New-ScheduledTaskTrigger -Once -At (Get-Date).AddMinutes(2) `
    -RepetitionInterval (New-TimeSpan -Minutes 10)
  Register-ScheduledTask -TaskName 'aimem-sync' -Action $syncAction -Trigger $syncTrigger -Settings $settings -Force | Out-Null
}

function Install-User {
  # AIMEM_PREBUILT lets boot.ps1 hand over a binary it just downloaded,
  # matching install.sh. The in-repo path stays as the manual fallback.
  $prebuilt = if ($env:AIMEM_PREBUILT) { $env:AIMEM_PREBUILT }
              else { Join-Path $RepoDir 'bin\windows-amd64\aimem.exe' }
  New-Item -ItemType Directory -Force $BinDir | Out-Null
  # Sweep the copies earlier upgrades parked once nothing holds them.
  Get-ChildItem "$Exe.old*", "$Exe.failed*" -ErrorAction SilentlyContinue | ForEach-Object {
    Remove-Item $_.FullName -Force -ErrorAction SilentlyContinue
  }
  if (-not (Test-Path $prebuilt)) {
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
      throw 'no prebuilt binary (bin\windows-amd64\aimem.exe) and no Go toolchain'
    }
    Say 'building aimem'
    $prebuilt = Join-Path $BinDir 'aimem.new.exe'
    Push-Location $RepoDir
    try { $env:CGO_ENABLED = '0'; go build -o $prebuilt ./cmd/aimem } finally { Pop-Location }
  }
  Register-AimemTasks
  if (Test-Path $Exe) {
    # An existing installation: back up its state, swap, and roll back
    # binary and state together if the new release does not come up.
    Invoke-Upgrade $Exe $prebuilt
  } else {
    Say 'installing prebuilt binary'
    Copy-Item $prebuilt $Exe -Force
    Start-AimemService
    Start-Sleep -Milliseconds 800
    try { & $Exe health | Out-Null; Say 'service healthy' }
    catch { Write-Warning 'service not answering yet; check: Get-ScheduledTask aimem-serve' }
  }
  Remove-Item (Join-Path $BinDir 'aimem.new.exe') -Force -ErrorAction SilentlyContinue
  Say "installed $Exe"

  # user PATH
  $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
  if (($userPath -split ';') -notcontains $BinDir) {
    [Environment]::SetEnvironmentVariable('Path', "$userPath;$BinDir", 'User')
    Say "added $BinDir to user PATH (new shells only)"
  }
  $env:Path = "$env:Path;$BinDir"

  Say "Claude Code user hooks -> $ClaudeSettings"
  Add-AgentHook $ClaudeSettings 'Stop'        $SubmitCmd 'Checkpointing turn'            'aimem submit-claude'
  Add-AgentHook $ClaudeSettings 'StopFailure' $SubmitCmd 'Checkpointing failed turn'     'aimem submit-claude'
  Add-AgentHook $ClaudeSettings 'PreCompact'  $SubmitCmd 'Journaling compaction marker'  'aimem submit-claude'

  # The plugin supports OpenCode 1.18+ and 2.x from one file; 1.x loaders
  # before 1.14 call every export as a function and stop OpenCode at
  # startup on it. No opencode, an unreadable version, or a 0.0.0-<tag> snapshot
  # build (current code, not an old release): install as before.
  $ocOld = $null
  $oc = Get-Command opencode -ErrorAction SilentlyContinue
  if ($oc) {
    # Same PS 5.1 trap as the codex calls below: relax EAP around the
    # native call, or any stderr from it would abort the whole install.
    $prevEap = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    $raw = ''
    try { $raw = (& $oc.Source --version 2>$null | Out-String) } catch { }
    $ErrorActionPreference = $prevEap
    if ($raw -match '(\d+)\.(\d+)(\.\d+)?' -and $Matches[0] -notin @('0.0.0', '0.0')) {
      $maj = [int]$Matches[1]; $min = [int]$Matches[2]
      if ($maj -eq 0 -or ($maj -eq 1 -and $min -lt 18)) { $ocOld = $Matches[0] }
    }
  }
  if ($ocOld) {
    Write-Warning ("OpenCode $ocOld is older than 1.18, the oldest release this aimem plugin supports " +
      "(before 1.14 it would stop OpenCode at startup), so $OcPluginDir\aimem.ts was not installed or updated. " +
      'Upgrade OpenCode, then re-run this install.')
  } else {
    Say "OpenCode global plugin -> $OcPluginDir\aimem.ts"
    New-Item -ItemType Directory -Force $OcPluginDir | Out-Null
    Copy-Item (Join-Path $RepoDir '.opencode\plugin\aimem.ts') (Join-Path $OcPluginDir 'aimem.ts') -Force
  }

  # Codex CLI: same checkpoint hooks, user-level (loads regardless of
  # project trust; Codex has no StopFailure). Wired even when codex is
  # absent — inert config until Codex reads it, like the other clients.
  $codexHooks = Join-Path $CodexHome 'hooks.json'
  Say "Codex user hooks -> $codexHooks"
  Add-AgentHook $codexHooks 'Stop'       $CodexSubmitCmd 'Checkpointing turn'           'aimem submit-codex'
  Add-AgentHook $codexHooks 'PreCompact' $CodexSubmitCmd 'Journaling compaction marker' 'aimem submit-codex'
  # MCP recall facade: Codex registers MCP servers globally in
  # ~/.codex/config.toml. Prefer the official CLI writer; the guarded
  # TOML append is the fallback BOTH when codex is off PATH and when
  # `codex mcp add` fails. CAUTION: under this script's EAP=Stop, a
  # `2>$null` on a native command turns its expected stderr ("server
  # not registered") into a TERMINATING error in PS 5.1 — the codex
  # calls run under a locally relaxed preference for that reason.
  $mcpDone = $false
  if (Get-Command codex -ErrorAction SilentlyContinue) {
    $prevEap = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    $null = codex mcp get aimem 2>$null
    $mcpDone = ($LASTEXITCODE -eq 0)
    if (-not $mcpDone) {
      $null = codex mcp add aimem -- aimem mcp 2>$null
      $mcpDone = ($LASTEXITCODE -eq 0)
    }
    $ErrorActionPreference = $prevEap
    if (-not $mcpDone) { Write-Warning 'codex mcp add failed; falling back to config.toml append' }
  }
  if (-not $mcpDone) {
    $codexToml = Join-Path $CodexHome 'config.toml'
    # Match hand-written spellings too (quoted key, inline table): a
    # missed match would append a duplicate key and break the whole
    # config.toml parse. A false positive merely skips the append.
    $haveEntry = (Test-Path $codexToml) -and ((Get-Content $codexToml -Raw) -match '(?m)^\[mcp_servers\."?aimem"?\]|^\s*"?aimem"?\s*=\s*\{')
    if (-not $haveEntry) {
      New-Item -ItemType Directory -Force $CodexHome | Out-Null
      [System.IO.File]::AppendAllText($codexToml, "`n[mcp_servers.aimem]`ncommand = `"aimem`"`nargs = [`"mcp`"]`n", $Utf8NoBom)
    }
  }
  Say 'Codex wired (first Codex run will ask once to trust the new hooks)'

  & $Exe spool-flush 2>$null | Out-Null
  Say 'user install done. Restart running OpenCode, Claude Code, and Codex sessions to activate.'
}

# BEGIN wire-decision
# Test-HomeDir: is $dir the user's home directory?
function Test-HomeDir($dir) {
  $a = (Resolve-Path -LiteralPath $dir).ProviderPath.TrimEnd('\', '/')
  $b = (Resolve-Path -LiteralPath $env:USERPROFILE).ProviderPath.TrimEnd('\', '/')
  return [string]::Equals($a, $b, [StringComparison]::OrdinalIgnoreCase)
}
# Get-WireSkipReason: '' when the bootstrap wires $dir as a project,
# otherwise why it does not. An upgrade wires only a project that already
# has its .aimem.json, so the one-liner can upgrade from any directory; a
# fresh install wires the directory it runs in.
function Get-WireSkipReason($dir, $wasInstalled) {
  if ($env:AIMEM_USER_ONLY -eq '1') { return 'AIMEM_USER_ONLY=1' }
  if (Test-HomeDir $dir) { return "$dir is the home directory, which is never wired as a project" }
  if ($wasInstalled -and -not (Test-Path -LiteralPath (Join-Path $dir '.aimem.json'))) {
    return "aimem was already installed and $dir has no .aimem.json"
  }
  return ''
}
# END wire-decision

function Assert-NotHome($dir) {
  if (Test-HomeDir $dir) {
    throw "refusing to wire the home directory $dir as a project: Claude Code would read its CLAUDE.md in every session under it. Run this inside a project directory."
  }
}

function Wire-Project($dir) {
  $dir = (Resolve-Path $dir).Path
  Assert-NotHome $dir
  Say "wiring project $dir"

  $handoff = Join-Path $dir 'docs\SESSION-STATE.md'
  if (-not (Test-Path $handoff)) {
    New-Item -ItemType Directory -Force (Join-Path $dir 'docs') | Out-Null
    Write-Text $handoff @"
# Session State

Updated: (date) | branch: (branch) | HEAD: (sha) | by: (client/session)

## Objective

(current objective)

## Next actions (ready)

1. (first action)

## Pick up here

(one line)
"@
    Say 'created docs/SESSION-STATE.md template'
  }

  # Claude Code: SessionStart handoff + project MCP registration.
  $ps = Join-Path $dir '.claude\settings.json'
  Add-AgentHook $ps 'SessionStart' $SessionStartCmd 'Loading session handoff' 'aimem session-start'
  Say 'Claude Code SessionStart handoff hook wired'

  # Codex: same handoff at session start, project-scoped (.codex/hooks.json;
  # loads once the user trusts the project). `aimem session-start` emits
  # the wire format both clients share.
  Add-AgentHook (Join-Path $dir '.codex\hooks.json') 'SessionStart' $SessionStartCmd 'Loading session handoff' 'aimem session-start'
  Say 'Codex SessionStart handoff hook wired'

  $mcpPath = Join-Path $dir '.mcp.json'
  $mcp = Read-Json $mcpPath
  if (-not $mcp.PSObject.Properties['mcpServers']) { $mcp | Add-Member mcpServers ([pscustomobject]@{}) }
  if (-not $mcp.mcpServers.PSObject.Properties['aimem']) {
    $mcp.mcpServers | Add-Member aimem ([pscustomobject]@{ command = 'aimem'; args = @('mcp') })
    Write-Json $mcpPath $mcp
  }

  # OpenCode: handoff instructions + MCP.
  $ocPath = Join-Path $dir 'opencode.json'
  $oc = Read-Json $ocPath
  if (-not $oc.PSObject.Properties['$schema']) { $oc | Add-Member '$schema' 'https://opencode.ai/config.json' }
  if (-not $oc.PSObject.Properties['instructions']) { $oc | Add-Member instructions @() }
  if (@($oc.instructions) -notcontains 'docs/SESSION-STATE.md') { $oc.instructions = @($oc.instructions) + 'docs/SESSION-STATE.md' }
  if (-not $oc.PSObject.Properties['mcp']) { $oc | Add-Member mcp ([pscustomobject]@{}) }
  if (-not $oc.mcp.PSObject.Properties['aimem']) {
    $oc.mcp | Add-Member aimem ([pscustomobject]@{ type = 'local'; command = @('aimem', 'mcp'); enabled = $true })
  }
  Write-Json $ocPath $oc
  Say 'OpenCode instructions + MCP wired'

  # /join_team entry points (Claude Code skill, OpenCode command, Codex skill
  # and custom prompt) are rendered by the binary from one source, so they
  # refresh on upgrade. Managed files; `aimem teams setup` repeats this.
  if (Get-Command aimem -ErrorAction SilentlyContinue) {
    & aimem teams commands $dir | Out-Null
    Say '/join_team entry points written (Claude Code, OpenCode, Codex)'
  }

  if (-not (Test-Path (Join-Path $dir 'AGENTS.md'))) {
    Copy-Item (Join-Path $RepoDir 'templates\AGENTS.md') (Join-Path $dir 'AGENTS.md')
    Say 'copied AGENTS.md protocol (edit its project-context section)'
  }
  if (-not (Test-Path (Join-Path $dir 'CLAUDE.md'))) {
    Write-Text (Join-Path $dir 'CLAUDE.md') "See @AGENTS.md for the shared session handoff protocol.`r`n"
    Say 'created CLAUDE.md import stub'
  }

  # Group membership: sharing is opt-in, empty groups = isolated project.
  # AIMEM_GROUPS="a,b" pre-declares shared knowledge groups at install time.
  $aimemJson = Join-Path $dir '.aimem.json'
  if (-not (Test-Path $aimemJson)) {
    $groups = @()
    if ($env:AIMEM_GROUPS) {
      $groups = @($env:AIMEM_GROUPS -split ',' | ForEach-Object { $_.Trim() } | Where-Object { $_ })
    }
    Write-Json $aimemJson ([pscustomobject]@{ groups = $groups })
    $gdesc = if ($groups.Count) { $groups -join ',' } else { 'none' }
    Say "created .aimem.json (groups: $gdesc - edit to join shared knowledge groups)"
  }
  Say 'project wired. Commit the new files if the project is tracked.'
}

function Uninstall-User {
  Unregister-ScheduledTask -TaskName 'aimem-serve' -Confirm:$false -ErrorAction SilentlyContinue
  Unregister-ScheduledTask -TaskName 'aimem-sync' -Confirm:$false -ErrorAction SilentlyContinue
  Remove-Item $Exe -ErrorAction SilentlyContinue
  Remove-Item (Join-Path $OcPluginDir 'aimem.ts') -ErrorAction SilentlyContinue
  if (Test-Path $ClaudeSettings) {
    $s = Read-Json $ClaudeSettings
    if ($s.PSObject.Properties['hooks']) {
      foreach ($ev in @($s.hooks.PSObject.Properties.Name)) {
        $kept = @($s.hooks.$ev | Where-Object {
          -not (@($_.hooks | Where-Object { "$($_.command)" -match 'aimem submit-claude' }).Count)
        })
        if ($kept.Count) { $s.hooks.$ev = $kept } else { $s.hooks.PSObject.Properties.Remove($ev) }
      }
      Write-Json $ClaudeSettings $s
    }
  }
  $codexHooks = Join-Path $CodexHome 'hooks.json'
  if (Test-Path $codexHooks) {
    $s = Read-Json $codexHooks
    if ($s.PSObject.Properties['hooks']) {
      foreach ($ev in @($s.hooks.PSObject.Properties.Name)) {
        $kept = @($s.hooks.$ev | Where-Object {
          -not (@($_.hooks | Where-Object { "$($_.command)" -match 'aimem submit-codex' }).Count)
        })
        if ($kept.Count) { $s.hooks.$ev = $kept } else { $s.hooks.PSObject.Properties.Remove($ev) }
      }
      Write-Json $codexHooks $s
    }
  }
  if (Get-Command codex -ErrorAction SilentlyContinue) {
    # Same PS 5.1 trap as in Install-User: relax EAP around the native
    # call or the expected "no such server" stderr becomes terminating.
    $prevEap = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    $null = codex mcp remove aimem 2>$null
    $ErrorActionPreference = $prevEap
  }
  # The TOML-append fallback registration must come out here too — the
  # CLI removal above only runs when codex is on PATH, which is exactly
  # the case in which the fallback was NOT used.
  $codexToml = Join-Path $CodexHome 'config.toml'
  if (Test-Path $codexToml) {
    $t = Get-Content $codexToml -Raw
    $t2 = [regex]::Replace($t, '(?ms)^\[mcp_servers\.aimem\]\r?\n.*?(?=^\[|\z)', '')
    if ($t2 -ne $t) { [System.IO.File]::WriteAllText($codexToml, $t2, $Utf8NoBom) }
  }
  Say 'user install removed (journal data left untouched)'
}

# Test-Older: is installed release $have older than $want? Build suffixes
# are ignored; an empty $have (binary too old for `aimem version`) counts as
# older; a $have with no numeric part (dev) is unknown and never does.
function Test-Older($have, $want) {
  if (-not $want) { return $false }
  if (-not $have) { return $true }
  $a = ($have -replace '^v', '') -replace '[^0-9.].*$', ''
  $b = ($want -replace '^v', '') -replace '[^0-9.].*$', ''
  if (-not $a) { return $false }
  try { return ([Version]$a -lt [Version]$b) } catch { return $false }
}

if ($UninstallUser) { Uninstall-User; return }
if (-not $UserOnly -and -not $Target) { $Target = (Get-Location).Path }
# An explicit project target is refused before anything is installed.
if (-not $UserOnly -and -not $Bootstrap) { Assert-NotHome $Target }
$wasInstalled = [bool](Get-Command aimem -ErrorAction SilentlyContinue) -or (Test-Path $Exe)
$have = ''
if (Get-Command aimem -ErrorAction SilentlyContinue) {
  try { $have = "$((& aimem version 2>$null) -split '\s+' | Select-Object -Index 1)" } catch { $have = '' }
}
if (-not (Get-Command aimem -ErrorAction SilentlyContinue) -or $env:AIMEM_REINSTALL -eq '1' -or $UserOnly) {
  Install-User
} elseif (Test-Older $have $env:AIMEM_TARGET_VERSION) {
  Say "installed aimem $have is older than $($env:AIMEM_TARGET_VERSION); upgrading"
  Install-User
} else {
  Say "aimem $have already on PATH and not older than the release; skipping user install (set AIMEM_REINSTALL=1 to force)"
}
if ($env:AIMEM_HUB_URL -and $env:AIMEM_HUB_TOKEN) {
  & $Exe hub $env:AIMEM_HUB_URL $env:AIMEM_HUB_TOKEN
  Say "hub push configured: $env:AIMEM_HUB_URL"
}
if ($UserOnly) { return }
$skip = if ($Bootstrap) { Get-WireSkipReason $Target $wasInstalled } else { '' }
if ($skip) {
  Say "user level only, no project wired: $skip"
  Say "to wire a project, run the one-liner inside it after creating its .aimem.json: Set-Content -Encoding ascii .aimem.json '{`"groups`":[]}'"
  return
}
Wire-Project $Target
