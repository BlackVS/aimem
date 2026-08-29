# aimem installer for Windows. Idempotent; safe to re-run.
# Usually invoked by boot.ps1 (the one-liner), but can be run directly:
#
#   .\install.ps1 -Target C:\path\to\project      # user install (if needed) + wire project
#   .\install.ps1 -UserOnly                       # user-level install only
#   .\install.ps1 -UninstallUser
#
# User install: aimem.exe -> %LOCALAPPDATA%\aimem\bin (added to user PATH),
# Claude Code checkpoint hooks, OpenCode global plugin, and a logon
# scheduled task running `aimem serve` (headless via conhost).
# NOTE: Windows support is best-effort; the service uses an AF_UNIX socket
# (supported on Windows 10 1803+ / Go 1.23+ std). Report issues.
param(
  [string]$Target,
  [switch]$UserOnly,
  [switch]$UninstallUser
)
$ErrorActionPreference = 'Stop'
$RepoDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$BinDir = Join-Path $env:LOCALAPPDATA 'aimem\bin'
$Exe = Join-Path $BinDir 'aimem.exe'
$ClaudeSettings = Join-Path $env:USERPROFILE '.claude\settings.json'
$OcPluginDir = Join-Path $env:USERPROFILE '.config\opencode\plugins'
$SubmitCmd = 'aimem submit-claude'
$SessionStartCmd = 'aimem session-start'

function Say($m) { Write-Host "==> $m" }

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
# re-runs find it. Never touches other hooks.
function Add-ClaudeHook($file, $event, $cmd, $status, $marker) {
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

function Install-User {
  # AIMEM_PREBUILT lets boot.ps1 hand over a binary it just downloaded,
  # matching install.sh. The in-repo path stays as the manual fallback.
  $prebuilt = if ($env:AIMEM_PREBUILT) { $env:AIMEM_PREBUILT }
              else { Join-Path $RepoDir 'bin\windows-amd64\aimem.exe' }
  New-Item -ItemType Directory -Force $BinDir | Out-Null
  # A running aimem.exe (the aimem-serve task) blocks Copy-Item, but
  # Windows allows RENAMING an in-use exe — same trick as the Linux
  # `cp new && mv` swap. Park the old file, drop the new one in, and
  # sweep parked copies on the next run once nothing holds them.
  # Park under a UNIQUE name: a fixed .old can still be locked by a
  # long-lived process from a previous upgrade (e.g. an `aimem mcp`
  # stdio server inside a running agent session), which blocks both the
  # delete and the rename-over-it. Sweep whatever is no longer held.
  Get-ChildItem "$Exe.old*" -ErrorAction SilentlyContinue | ForEach-Object {
    Remove-Item $_.FullName -Force -ErrorAction SilentlyContinue
  }
  if (Test-Path $Exe) {
    $park = "$Exe.old-" + (Get-Date -Format 'yyyyMMddHHmmss')
    try { Move-Item $Exe $park } catch {
      throw "cannot replace $Exe (still locked even for rename): $_"
    }
  }
  if (Test-Path $prebuilt) {
    Say 'installing prebuilt binary'
    Copy-Item $prebuilt $Exe -Force
  } elseif (Get-Command go -ErrorAction SilentlyContinue) {
    Say 'building aimem'
    Push-Location $RepoDir
    try { $env:CGO_ENABLED = '0'; go build -o $Exe ./cmd/aimem } finally { Pop-Location }
  } else {
    throw 'no prebuilt binary (bin\windows-amd64\aimem.exe) and no Go toolchain'
  }
  Say "installed $Exe"

  # user PATH
  $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
  if (($userPath -split ';') -notcontains $BinDir) {
    [Environment]::SetEnvironmentVariable('Path', "$userPath;$BinDir", 'User')
    Say "added $BinDir to user PATH (new shells only)"
  }
  $env:Path = "$env:Path;$BinDir"

  Say "Claude Code user hooks -> $ClaudeSettings"
  Add-ClaudeHook $ClaudeSettings 'Stop'        $SubmitCmd 'Checkpointing turn'            'aimem submit-claude'
  Add-ClaudeHook $ClaudeSettings 'StopFailure' $SubmitCmd 'Checkpointing failed turn'     'aimem submit-claude'
  Add-ClaudeHook $ClaudeSettings 'PreCompact'  $SubmitCmd 'Journaling compaction marker'  'aimem submit-claude'

  Say "OpenCode global plugin -> $OcPluginDir\aimem.ts"
  New-Item -ItemType Directory -Force $OcPluginDir | Out-Null
  Copy-Item (Join-Path $RepoDir '.opencode\plugin\aimem.ts') (Join-Path $OcPluginDir 'aimem.ts') -Force

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

  # Restart, not start: a running task would keep serving the old
  # (renamed) binary; Start on a Running task is a no-op. And stopping
  # the TASK is not enough: it kills the conhost wrapper but ORPHANS
  # its aimem child, which keeps serving the parked binary through the
  # socket — found live when a service still reported a three-releases-
  # old version after "successful" upgrades. Kill stray serve processes
  # explicitly.
  Stop-ScheduledTask 'aimem-serve' -ErrorAction SilentlyContinue
  Get-CimInstance Win32_Process -Filter "Name='aimem.exe'" |
    Where-Object { $_.CommandLine -match 'serve' } |
    ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }
  Start-ScheduledTask 'aimem-serve'
  Start-Sleep -Milliseconds 800
  try { & $Exe health | Out-Null; Say 'service healthy' }
  catch { Write-Warning 'service not answering yet; check: Get-ScheduledTask aimem-serve' }
  & $Exe spool-flush 2>$null | Out-Null
  Say 'user install done. Restart running OpenCode and Claude Code sessions to activate.'
}

function Wire-Project($dir) {
  $dir = (Resolve-Path $dir).Path
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
  Add-ClaudeHook $ps 'SessionStart' $SessionStartCmd 'Loading session handoff' 'aimem session-start'
  Say 'Claude Code SessionStart handoff hook wired'

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
  Say 'user install removed (journal data left untouched)'
}

if ($UninstallUser) { Uninstall-User; return }
if (-not (Get-Command aimem -ErrorAction SilentlyContinue) -or $env:AIMEM_REINSTALL -eq '1' -or $UserOnly) {
  Install-User
} else {
  Say "aimem already on PATH; skipping user install (set AIMEM_REINSTALL=1 to force)"
}
if ($env:AIMEM_HUB_URL -and $env:AIMEM_HUB_TOKEN) {
  & $Exe hub $env:AIMEM_HUB_URL $env:AIMEM_HUB_TOKEN
  Say "hub push configured: $env:AIMEM_HUB_URL"
}
if (-not $UserOnly) {
  if (-not $Target) { $Target = (Get-Location).Path }
  Wire-Project $Target
}
