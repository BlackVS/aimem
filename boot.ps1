# aimem bootstrap for Windows. A first install runs it inside a project
# directory; an upgrade runs it from anywhere.
#
#   powershell -NoProfile -ExecutionPolicy Bypass -Command "irm https://raw.githubusercontent.com/BlackVS/aimem/v0.10.0/boot.ps1 | iex"
#
# (The wrapper survives the default Restricted execution policy on a
# fresh Windows machine; bare `irm ... | iex` works too once fetched,
# since this script bypasses the policy for the installer it runs.)
#
# It installs the release this script was fetched from ($release below):
# the prebuilt aimem.exe (no Go needed), checked against that release's
# SHA256SUMS, and the repository archive of the same tag for the installer
# and the OpenCode plugin. Then it runs install.ps1 against the current
# directory. An upgrade backs up the state root first and rolls back if
# the new release does not come up (install.ps1). When aimem was already
# installed, only a directory that already holds .aimem.json is wired; the
# home directory is never wired.
#
# Optional environment:
#   AIMEM_HUB_URL, AIMEM_HUB_TOKEN   register a hub for real-time push
#   AIMEM_GROUPS=a,b                 pre-declare shared knowledge groups
#   AIMEM_USER_ONLY=1                install or upgrade the user level only
#                                    and wire no project
#   AIMEM_REINSTALL=1                refresh the binary and hooks even if
#                                    the installed aimem is already current
#                                    (an older install is upgraded anyway)
#   AIMEM_REPO=owner/name            install from a fork
#   AIMEM_VERSION=vX.Y.Z             install another release than $release
#   AIMEM_UPGRADE_WAIT=30            seconds an upgrade waits for health at
#                                    the new version before rolling back
$ErrorActionPreference = 'Stop'

# The release this script installs. Bumped together with the CHANGELOG
# when a release is cut (internal/installer checks they agree).
$release = 'v0.10.0'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$repo = if ($env:AIMEM_REPO) { $env:AIMEM_REPO } else { 'BlackVS/aimem' }
$base = "https://github.com/$repo"

$tag = if ($env:AIMEM_VERSION) { $env:AIMEM_VERSION } else { $release }
# Tell install.ps1 which release this is, so an older install gets upgraded.
$env:AIMEM_TARGET_VERSION = $tag

$dest = Join-Path ([IO.Path]::GetTempPath()) ("aimem-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force $dest | Out-Null
try {
  Write-Host "Fetching aimem $tag ..."
  $archive = "$base/archive/refs/tags/$tag.tar.gz"
  $tgz = Join-Path $dest 'src.tar.gz'
  Invoke-WebRequest $archive -OutFile $tgz -UseBasicParsing
  # tar ships with Windows 10 1803+ and understands .tar.gz.
  & tar -xzf $tgz -C $dest --strip-components=1
  Remove-Item $tgz

  $prebuilt = Join-Path $dest 'aimem-prebuilt.exe'
  try {
    Invoke-WebRequest "$base/releases/download/$tag/aimem-windows-amd64.exe" `
      -OutFile $prebuilt -UseBasicParsing
    $env:AIMEM_PREBUILT = $prebuilt
  } catch {
    Write-Warning "No prebuilt aimem.exe in release $tag; building from source (needs Go)."
    Remove-Item -Force $prebuilt -ErrorAction SilentlyContinue
  }
  # Verify against the release's SHA256SUMS. Deliberately OUTSIDE the
  # try/catch above: a missing sums file or a mismatch must abort the
  # install, never degrade into the source-build fallback (that path is
  # reserved for a release with no binary at all). Guard on the file we
  # actually downloaded — NOT $env:AIMEM_PREBUILT, which is also a
  # user-supplied knob that survives a failed download and would send
  # Get-FileHash at a path that does not exist.
  if (Test-Path $prebuilt) {
    $sums = Join-Path $dest 'SHA256SUMS'
    Invoke-WebRequest "$base/releases/download/$tag/SHA256SUMS" -OutFile $sums -UseBasicParsing
    $want = (Select-String -Path $sums -Pattern 'aimem-windows-amd64\.exe$' |
             ForEach-Object { ($_.Line -split '\s+')[0] } | Select-Object -First 1)
    $got = (Get-FileHash $prebuilt -Algorithm SHA256).Hash.ToLower()
    if (-not $want -or $want.ToLower() -ne $got) {
      throw "checksum mismatch for aimem-windows-amd64.exe (want $want, got $got)"
    }
    Write-Host "checksum OK: aimem-windows-amd64.exe"
  }

  # Run the installer in a child shell with an explicit policy bypass:
  # `irm | iex` itself is exempt from ExecutionPolicy, but invoking the
  # downloaded install.ps1 as a FILE is not — on the default Restricted
  # policy the install died right here (lived 2026-08-31). The bypass is
  # process-scoped and changes no machine state.
# BEGIN run-installer
  # -Bootstrap applies the one-liner's wiring rules; an older release named
  # by AIMEM_VERSION has an install.ps1 without it.
  $installer = Join-Path $dest 'install.ps1'
  $mode = @()
  if (Select-String -LiteralPath $installer -Pattern '\[switch\]\$Bootstrap' -Quiet) { $mode = @('-Bootstrap') }
  & powershell -NoProfile -ExecutionPolicy Bypass -File $installer @mode -Target $PWD.Path
  if ($LASTEXITCODE -ne 0) { throw "install.ps1 failed with exit code $LASTEXITCODE" }
# END run-installer
} finally {
  Remove-Item -Recurse -Force $dest -ErrorAction SilentlyContinue
}
