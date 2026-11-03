# truner "standard" profile for Windows Server runners: Git (with bash),
# PowerShell 7, long paths and Defender exclusions for build directories.
$ErrorActionPreference = "Continue"
$ProgressPreference = "SilentlyContinue"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

function Write-Warn($msg) { Write-Output "::truner-warning:: $msg" }

function Get-LatestAsset([string]$repo, [string]$pattern) {
  $headers = @{ "User-Agent" = "truner"; "Accept" = "application/vnd.github+json" }
  $rel = Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases/latest" -Headers $headers
  return ($rel.assets | Where-Object { $_.name -match $pattern } | Select-Object -First 1).browser_download_url
}

function Download([string]$url, [string]$out) {
  for ($i = 0; $i -lt 5; $i++) {
    try { Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $out; return $true } catch { Start-Sleep -Seconds 3 }
  }
  return $false
}

New-ItemProperty -Path "HKLM:\SYSTEM\CurrentControlSet\Control\FileSystem" -Name "LongPathsEnabled" -Value 1 -PropertyType DWORD -Force | Out-Null

try {
  Add-MpPreference -ExclusionPath "C:\actions-runner", "C:\truner", "C:\hostedtoolcache", "$env:TEMP" -ErrorAction Stop
} catch { Write-Warn "could not add Defender exclusions: $_" }

if (-not (Test-Path "C:\Program Files\Git\cmd\git.exe")) {
  try {
    $url = Get-LatestAsset "git-for-windows/git" "^Git-[0-9.]+-64-bit\.exe$"
    $exe = Join-Path $env:TEMP "git-setup.exe"
    if (Download $url $exe) {
      Start-Process -FilePath $exe -ArgumentList "/VERYSILENT", "/NORESTART", "/NOCANCEL", "/SP-", "/SUPPRESSMSGBOXES", "/o:PathOption=CmdTools" -Wait
    }
  } catch { Write-Warn "git install failed: $_" }
}

if (-not (Test-Path "C:\Program Files\PowerShell\7\pwsh.exe")) {
  try {
    $url = Get-LatestAsset "PowerShell/PowerShell" "^PowerShell-[0-9.]+-win-x64\.msi$"
    $msi = Join-Path $env:TEMP "pwsh.msi"
    if (Download $url $msi) {
      Start-Process -FilePath "msiexec.exe" -ArgumentList "/i", $msi, "/quiet", "/norestart", "ADD_PATH=1", "ENABLE_PSREMOTING=0" -Wait
    }
  } catch { Write-Warn "PowerShell 7 install failed: $_" }
}

& "C:\Program Files\Git\cmd\git.exe" config --system core.longpaths true 2>$null
New-Item -ItemType Directory -Force -Path "C:\hostedtoolcache\windows" | Out-Null
Write-Output "windows profile done"
