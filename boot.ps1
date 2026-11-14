# aimem bootstrap for Windows. A first install runs it inside a project
# directory; an upgrade runs it from anywhere.
#
#   Invoke-WebRequest https://github.com/BlackVS/aimem/releases/download/v0.10.22/boot.ps1 -OutFile $f -UseBasicParsing
#   (check $f against the same release's SHA256SUMS, then Unblock-File $f)
#   powershell -NoProfile -ExecutionPolicy RemoteSigned -File $f
#
# In a closed network, from an aiforge export (a local directory, a share
# or an internal https server holding the release files):
#
#   $env:AIMEM_BASE = 'D:\aiforge-export'
#   powershell -NoProfile -ExecutionPolicy RemoteSigned -File D:\aiforge-export\aimem\<tag>\boot.ps1
#
# README.md has the documented block. Run as `irm ... | iex` it works too,
# in the caller's own process.
#
# It installs the release this script was fetched from ($release below), or
# the one the export's MANIFEST names: the prebuilt aimem.exe, the
# installer (install.ps1) and the OpenCode plugin, each a release asset
# checked against that release's SHA256SUMS before it is used. Then it runs
# install.ps1 against the current directory. An upgrade backs up the state
# root first and rolls back if the new release does not come up
# (install.ps1). When aimem was already installed, only a directory that
# already holds .aimem.json is wired; the home directory is never wired.
#
# A release named by AIMEM_VERSION that was published before its installer
# files became release assets (its SHA256SUMS lists no install.ps1) is
# installed by that release's own boot.ps1, fetched from the repository at
# its tag and run in this process as `irm | iex` runs it, with the same
# environment, as its own one-liner would. An export holds only releases
# with the assets, so with AIMEM_BASE there is no hand-over.
#
# Optional environment:
#   AIMEM_HUB_URL, AIMEM_HUB_TOKEN   register a hub for real-time push
#   AIMEM_GROUPS=a,b                 pre-declare shared knowledge groups
#   AIMEM_USER_ONLY=1                install or upgrade the user level only
#                                    and wire no project
#   AIMEM_REINSTALL=1                refresh the binary and hooks even if
#                                    the installed aimem is already current
#                                    (an older install is upgraded anyway)
#   AIMEM_BASE=X:\dir|\\host\share|https://...
#                                    install from an aiforge export instead
#                                    of github.com: <base>\MANIFEST names
#                                    the release, <base>\aimem\<tag>\ holds
#                                    its files; no other host is contacted
#   AIMEM_REPO=owner/name            install from a fork
#   AIMEM_VERSION=vX.Y.Z             install another release than $release
#                                    (or than the export's MANIFEST names)
#   AIMEM_UPGRADE_WAIT=30            seconds an upgrade waits for health at
#                                    the new version before rolling back
#
# Set a knob just before the run and remove it in a `finally`, so it lives
# only for that run (docs/ADMIN-MANUAL.md, Upgrades, has the form). Set in
# the caller's window and left there, it would stay for every later run.

# The body runs in a script block of its own: run as `irm | iex`, the
# script runs in the caller's scope, and its variables, among them
# $ErrorActionPreference = 'Stop', would stay set in that window. The body
# is not indented, so the markers and the $release line keep their shape.
& {
$ErrorActionPreference = 'Stop'

# The release this script installs. Bumped together with the CHANGELOG
# when a release is cut (internal/installer checks they agree).
$release = 'v0.10.22'

# BEGIN release-source
# Where the release's files come from, and their verification; the same
# rules as boot.sh's release-source block. Connected: the release download
# URL on github.com. With AIMEM_BASE: an aiforge export, format 1 -
# <base>\MANIFEST is `aiforge-export 1` and then `<product> <tag>` lines,
# <base>\aimem\<tag>\ holds the release's assets under their published
# names with its SHA256SUMS. An explicit AIMEM_VERSION wins over MANIFEST.
# Every file is checked against SHA256SUMS before it is used; any failure
# stops with the path and installs nothing.
function Stop-Install($msg) { throw "$msg; nothing was installed." }
function Get-SourceFile($src, $dest) {
  if ($src -like 'https://*') {
    try { Invoke-WebRequest $src -OutFile $dest -UseBasicParsing } catch { Write-Warning "$src : $_"; return $false }
    return $true
  }
  if (-not (Test-Path -LiteralPath $src -PathType Leaf)) { return $false }
  Copy-Item -LiteralPath $src -Destination $dest
  return $true
}
function Test-Tag($t) { return ($t -cmatch '^v[0-9][A-Za-z0-9._+-]*$') -and ($t -notlike '*..*') }
function Resolve-Source($work, $repo, $release) {
  if (-not $env:AIMEM_BASE) {
    $t = if ($env:AIMEM_VERSION) { $env:AIMEM_VERSION } else { $release }
    if (-not (Test-Tag $t)) { Stop-Install "'$t' is not a release tag" }
    return @{ Tag = $t; Src = "https://github.com/$repo/releases/download/$t" }
  }
  $b = $env:AIMEM_BASE
  if (-not ($b -like 'https://*' -or $b -match '^[A-Za-z]:[\\/]' -or $b -match '^\\\\[^\\]')) {
    Stop-Install "AIMEM_BASE must be an absolute directory, a \\host\share path or an https:// URL, not '$b'"
  }
  $b = $b.TrimEnd('/', '\')
  if ($env:AIMEM_VERSION) {
    $t = $env:AIMEM_VERSION
  } else {
    $m = Join-Path $work 'MANIFEST'
    if (-not (Get-SourceFile "$b/MANIFEST" $m)) {
      Stop-Install "$b/MANIFEST is missing (or set AIMEM_VERSION to name the release)"
    }
    $lines = @(Get-Content -LiteralPath $m)
    $format = if ($lines.Count) { $lines[0].Trim() } else { '' }
    if ($format -cne 'aiforge-export 1') {
      Stop-Install "$b/MANIFEST is '$format', not a format this installer reads (aiforge-export 1)"
    }
    $t = $lines | Select-Object -Skip 1 | ForEach-Object { , (-split $_) } |
      Where-Object { $_.Count -ge 2 -and $_[0] -ceq 'aimem' } |
      ForEach-Object { $_[1] } | Select-Object -First 1
    if (-not $t) { Stop-Install "$b/MANIFEST names no aimem release" }
  }
  if (-not (Test-Tag $t)) { Stop-Install "'$t' is not a release tag" }
  return @{ Tag = $t; Src = "$b/aimem/$t" }
}
function Get-Verified($work, $src, $asset) {
  # The sums line is matched on the whole name, never a suffix of another.
  $sums = Join-Path $work 'SHA256SUMS'
  $out = Join-Path $work $asset
  if (-not (Get-SourceFile "$src/$asset" $out)) {
    Stop-Install "$src/$asset is missing (a release published before its installer files became release assets installs with its own boot script)"
  }
  $want = Get-Content -LiteralPath $sums | ForEach-Object { , (-split $_) } |
    Where-Object { $_.Count -ge 2 -and $_[1] -ceq $asset } |
    ForEach-Object { $_[0].ToLower() } | Select-Object -First 1
  $got = (Get-FileHash -LiteralPath $out -Algorithm SHA256).Hash.ToLower()
  if (-not $want -or $want -ne $got) {
    Stop-Install "checksum mismatch for $src/$asset (want $(if ($want) { $want } else { 'absent' }), got $got)"
  }
  Write-Host "checksum OK: $asset"
  return $out
}
# END release-source

$repo = if ($env:AIMEM_REPO) { $env:AIMEM_REPO } else { 'BlackVS/aimem' }

# The variables this script sets for install.ps1 (or, for AIMEM_VERSION, for
# an older release's boot script) are put back as they were
# when it ends: run as `irm | iex` in an open window, it runs in that
# window's process, and a stale AIMEM_PREBUILT is a knob the next run reads.
$saved = @{}
foreach ($k in 'AIMEM_TARGET_VERSION', 'AIMEM_PREBUILT', 'AIMEM_VERSION') {
  $saved[$k] = [Environment]::GetEnvironmentVariable($k, 'Process')
}
$dest = Join-Path ([IO.Path]::GetTempPath()) ("aimem-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force $dest | Out-Null
$protocol = [Net.ServicePointManager]::SecurityProtocol
try {
  # Process-wide, so the finally puts it back too.
  [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
  $source = Resolve-Source $dest $repo $release
  $tag = $source.Tag
  $src = $source.Src
  # Tell install.ps1 which release this is, so an older install gets upgraded.
  $env:AIMEM_TARGET_VERSION = $tag
  Write-Host "Fetching aimem $tag from $src ..."

  if (-not (Get-SourceFile "$src/SHA256SUMS" (Join-Path $dest 'SHA256SUMS'))) {
    Stop-Install "$src/SHA256SUMS is missing; refusing unverified files"
  }
  $listed = Get-Content -LiteralPath (Join-Path $dest 'SHA256SUMS') | ForEach-Object { , (-split $_) } |
    Where-Object { $_.Count -ge 2 -and $_[1] -ceq 'install.ps1' }
  if (-not $env:AIMEM_BASE -and -not $listed) {
    $url = "https://raw.githubusercontent.com/$repo/$tag/boot.ps1"
    Write-Host "aimem $tag has no installer assets; handing over to that release's own boot script ($url)"
    $old = Join-Path $dest "boot-$tag.ps1"
    if (-not (Get-SourceFile $url $old)) { Stop-Install "$url could not be fetched" }
    $env:AIMEM_VERSION = $tag
    & ([scriptblock]::Create((Get-Content -Raw -LiteralPath $old)))
    return
  }
  $exe = Get-Verified $dest $src 'aimem-windows-amd64.exe'
  $null = Get-Verified $dest $src 'install.ps1'
  $plugin = Get-Verified $dest $src 'aimem-opencode.ts'
  # install.ps1 reads the plugin beside itself, where a checkout has it.
  $pluginDir = Join-Path $dest '.opencode\plugin'
  New-Item -ItemType Directory -Force $pluginDir | Out-Null
  Move-Item -LiteralPath $plugin -Destination (Join-Path $pluginDir 'aimem.ts')
  $prebuilt = Join-Path $dest 'aimem-prebuilt.exe'
  Move-Item -LiteralPath $exe -Destination $prebuilt
  $env:AIMEM_PREBUILT = $prebuilt

  # Run the installer in a child shell with an explicit policy:
  # `irm | iex` itself is exempt from ExecutionPolicy, but invoking the
  # downloaded install.ps1 as a FILE is not - on the default Restricted
  # policy the install died right here (lived 2026-08-31). RemoteSigned
  # is enough: the installer was checked against the release's SHA256SUMS
  # above, so the Mark of the Web a copy may carry (a file taken from a
  # downloaded export) is cleared first. The policy is process-scoped and
  # changes no machine state.
# BEGIN run-installer
  # -Bootstrap applies the one-liner's wiring rules; an older release named
  # by AIMEM_VERSION has an install.ps1 without it.
  $installer = Join-Path $dest 'install.ps1'
  Unblock-File -LiteralPath $installer
  $mode = @()
  if (Select-String -LiteralPath $installer -Pattern '\[switch\]\$Bootstrap' -Quiet) { $mode = @('-Bootstrap') }
  & powershell -NoProfile -ExecutionPolicy RemoteSigned -File $installer @mode -Target $PWD.Path
  if ($LASTEXITCODE -ne 0) { throw "install.ps1 failed with exit code $LASTEXITCODE" }
# END run-installer
} finally {
  Remove-Item -Recurse -Force $dest -ErrorAction SilentlyContinue
  foreach ($k in $saved.Keys) {
    [Environment]::SetEnvironmentVariable($k, $saved[$k], 'Process')
  }
  [Net.ServicePointManager]::SecurityProtocol = $protocol
}
}
