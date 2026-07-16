# use-browser installer for Windows.
#   irm https://raw.githubusercontent.com/hoangvu12/use-browser/main/install.ps1 | iex
$ErrorActionPreference = "Stop"

$repo = "hoangvu12/use-browser"
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
$asset = "use-browser_windows_$arch.exe"
$url = "https://github.com/$repo/releases/latest/download/$asset"

$dir = Join-Path $env:LOCALAPPDATA "use-browser"
New-Item -ItemType Directory -Force $dir | Out-Null
$exe = Join-Path $dir "use-browser.exe"

Write-Host "downloading $url"
try {
    Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $exe
} catch {
    Write-Host "error: download failed. If no release exists yet, build from source:" -ForegroundColor Red
    Write-Host "  git clone https://github.com/$repo; cd use-browser; go build -o use-browser.exe ."
    exit 1
}
# add to user PATH if missing
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath -notlike "*$dir*") {
    [Environment]::SetEnvironmentVariable("Path", "$userPath;$dir", "User")
    Write-Host "added $dir to your user PATH (restart your terminal)"
}

Write-Host "installed $exe"
Write-Host "next: start Chrome with --remote-debugging-port=9222, then run: use-browser doctor"
