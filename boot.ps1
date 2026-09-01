# aimem bootstrap for Windows. Run it INSIDE a project directory.
#
#   powershell -NoProfile -ExecutionPolicy Bypass -Command "irm https://raw.githubusercontent.com/BlackVS/aimem/master/boot.ps1 | iex"
#
# (The wrapper survives the default Restricted execution policy on a
# fresh Windows machine; bare `irm ... | iex` works too once fetched,
# since this script bypasses the policy for the installer it runs.)
#
# It downloads the latest release (a prebuilt aimem.exe - no Go needed),
# unpacks the repository archive for the installer and the OpenCode plugin,
# and runs install.ps1 against the current directory.
#
# Optional environment:
#   AIMEM_HUB_URL, AIMEM_HUB_TOKEN   register a hub for real-time push
#   AIMEM_GROUPS=a,b                 pre-declare shared knowledge groups
#   AIMEM_REINSTALL=1                refresh the binary and hooks even if
#                                    aimem is already installed
#   AIMEM_REPO=owner/name            install from a fork
#   AIMEM_VERSION=vX.Y.Z             pin a release instead of the latest
$ErrorActionPreference = 'Stop'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$repo = if ($env:AIMEM_REPO) { $env:AIMEM_REPO } else { 'BlackVS/aimem' }
$base = "https://github.com/$repo"

$tag = $env:AIMEM_VERSION
if (-not $tag) {
  # /releases/latest redirects to the tag page; read the version off the
  # effective URL rather than parsing the API.
  try {
    $r = Invoke-WebRequest "$base/releases/latest" -MaximumRedirection 5 -UseBasicParsing
    if ($r.BaseResponse.ResponseUri.AbsoluteUri -match '/releases/tag/(.+)$') { $tag = $Matches[1] }
  } catch { $tag = $null }
}
if (-not $tag) { $tag = 'master' }   # no releases yet: install from the branch

$dest = Join-Path ([IO.Path]::GetTempPath()) ("aimem-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force $dest | Out-Null
try {
  Write-Host "Fetching aimem $tag ..."
  $archive = if ($tag -eq 'master') { "$base/archive/refs/heads/master.tar.gz" }
             else { "$base/archive/refs/tags/$tag.tar.gz" }
  $tgz = Join-Path $dest 'src.tar.gz'
  Invoke-WebRequest $archive -OutFile $tgz -UseBasicParsing
  # tar ships with Windows 10 1803+ and understands .tar.gz.
  & tar -xzf $tgz -C $dest --strip-components=1
  Remove-Item $tgz

  if ($tag -ne 'master') {
    $prebuilt = Join-Path $dest 'aimem-prebuilt.exe'
    try {
      Invoke-WebRequest "$base/releases/download/$tag/aimem-windows-amd64.exe" `
        -OutFile $prebuilt -UseBasicParsing
      $env:AIMEM_PREBUILT = $prebuilt
    } catch {
      Write-Warning "No prebuilt aimem.exe in release $tag; building from source (needs Go)."
      Remove-Item -Force $prebuilt -ErrorAction SilentlyContinue
    }
  }

  # Run the installer in a child shell with an explicit policy bypass:
  # `irm | iex` itself is exempt from ExecutionPolicy, but invoking the
  # downloaded install.ps1 as a FILE is not — on the default Restricted
  # policy the install died right here (lived 2026-08-31). The bypass is
  # process-scoped and changes no machine state.
  & powershell -NoProfile -ExecutionPolicy Bypass -File (Join-Path $dest 'install.ps1') -Target $PWD.Path
  if ($LASTEXITCODE -ne 0) { throw "install.ps1 failed with exit code $LASTEXITCODE" }
} finally {
  Remove-Item -Recurse -Force $dest -ErrorAction SilentlyContinue
}
