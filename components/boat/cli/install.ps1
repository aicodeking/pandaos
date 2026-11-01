# Install the boat CLI on Windows from a pandaos GitHub release.
#
#   irm https://raw.githubusercontent.com/aicodeking/pandaos/main/components/boat/cli/install.ps1 | iex
#
# Env: BOAT_VERSION (tag, default latest), BOAT_INSTALL_DIR (default
# %LOCALAPPDATA%\boat\bin), BOAT_REPO (default aicodeking/pandaos),
# BOAT_RELEASE_BASE (download base URL instead of GitHub).
# The binary is installed only if its SHA-256 matches boat_SHA256SUMS.
# Later upgrades: boat upgrade. ssh needs the Windows OpenSSH client.
$ErrorActionPreference = 'Stop'

$repo = if ($env:BOAT_REPO) { $env:BOAT_REPO } else { 'aicodeking/pandaos' }
$version = if ($env:BOAT_VERSION) { $env:BOAT_VERSION } else { 'latest' }
$dir = if ($env:BOAT_INSTALL_DIR) { $env:BOAT_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'boat\bin' }

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
  'AMD64' { 'amd64' }
  'ARM64' { 'arm64' }
  default { throw "install.ps1: unsupported CPU $($env:PROCESSOR_ARCHITECTURE)" }
}
$asset = "boat_windows_$arch.exe"

if ($env:BOAT_RELEASE_BASE) { $base = $env:BOAT_RELEASE_BASE.TrimEnd('/') }
elseif ($version -eq 'latest') { $base = "https://github.com/$repo/releases/latest/download" }
else { $base = "https://github.com/$repo/releases/download/$version" }

$tmp = Join-Path ([IO.Path]::GetTempPath()) ("boat-install-" + [Guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
  Invoke-WebRequest -UseBasicParsing "$base/boat_SHA256SUMS" -OutFile "$tmp\sums"
  Invoke-WebRequest -UseBasicParsing "$base/$asset" -OutFile "$tmp\boat.exe"

  $line = Get-Content "$tmp\sums" | Where-Object { ($_ -split '\s+')[1] -replace '^\*', '' -eq $asset } | Select-Object -First 1
  if (-not $line) { throw "install.ps1: $asset is not listed in boat_SHA256SUMS" }
  $want = ($line -split '\s+')[0].ToLower()
  $got = (Get-FileHash -Algorithm SHA256 "$tmp\boat.exe").Hash.ToLower()
  if ($got -ne $want) { throw "install.ps1: checksum mismatch for $asset (got $got, want $want)" }

  New-Item -ItemType Directory -Force -Path $dir | Out-Null
  Move-Item -Force "$tmp\boat.exe" "$dir\boat.exe"
  Write-Host "installed $dir\boat.exe ($version, $asset)"
  $path = [Environment]::GetEnvironmentVariable('Path', 'User')
  if (($path -split ';') -notcontains $dir) {
    [Environment]::SetEnvironmentVariable('Path', "$path;$dir", 'User')
    Write-Host "added $dir to your user Path; open a new terminal"
  }
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
