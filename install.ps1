# Installs the truner CLI on Windows.
#   irm https://raw.githubusercontent.com/codician-team/truner/main/install.ps1 | iex
# Options (environment): TRUNER_VERSION=v0.1.0  TRUNER_INSTALL_DIR=...  TRUNER_REPO=owner/name
$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"

$repo = if ($env:TRUNER_REPO) { $env:TRUNER_REPO } else { "codician-team/truner" }
$version = if ($env:TRUNER_VERSION) { $env:TRUNER_VERSION } else { "latest" }
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
$bin = "truner_windows_$arch.exe"
$base = if ($version -eq "latest") { "https://github.com/$repo/releases/latest/download" } else { "https://github.com/$repo/releases/download/$version" }

$dir = if ($env:TRUNER_INSTALL_DIR) { $env:TRUNER_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "truner\bin" }
New-Item -ItemType Directory -Force -Path $dir | Out-Null
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("truner-" + [Guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  Write-Host "Downloading $bin ($version)..."
  Invoke-WebRequest -UseBasicParsing "$base/$bin" -OutFile "$tmp\truner.exe"
  Invoke-WebRequest -UseBasicParsing "$base/checksums.txt" -OutFile "$tmp\checksums.txt"
  $want = (Select-String -Path "$tmp\checksums.txt" -Pattern " $([regex]::Escape($bin))$").Line.Split(" ")[0]
  $got = (Get-FileHash "$tmp\truner.exe" -Algorithm SHA256).Hash.ToLower()
  if (-not $want -or $want -ne $got) { throw "checksum mismatch for $bin" }
  Move-Item -Force "$tmp\truner.exe" (Join-Path $dir "truner.exe")
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (-not ($userPath -split ";" | Where-Object { $_ -eq $dir })) {
  [Environment]::SetEnvironmentVariable("Path", "$userPath;$dir", "User")
  $env:Path += ";$dir"
  Write-Host "Added $dir to your PATH (open a new terminal to use it everywhere)."
}
Write-Host "Installed truner to $dir\truner.exe"
Write-Host "Next: truner install"
