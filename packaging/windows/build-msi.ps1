# Builds k8s-buddy MSIs with WiX v5.
# Usage: packaging/windows/build-msi.ps1 -Version 1.2.3 -Arch x64 -BinDir build/amd64 -Out dist/k8s-buddy.msi
param(
	[Parameter(Mandatory = $true)][string] $Version,
	[Parameter(Mandatory = $true)][ValidateSet('x64', 'arm64')][string] $Arch,
	[Parameter(Mandatory = $true)][string] $BinDir,
	[Parameter(Mandatory = $true)][string] $Out
)
$ErrorActionPreference = 'Stop'
$wixVersion = '5.0.2'

if (-not (Get-Command wix -ErrorAction SilentlyContinue)) {
	dotnet tool install --global wix --version $wixVersion | Out-Host
	$env:PATH += ";$env:USERPROFILE\.dotnet\tools"
}
wix extension add -g "WixToolset.UI.wixext/$wixVersion" | Out-Host

# MSI versions are numeric: 1.2.3-rc.1 installs as 1.2.3.
$msiVersion = ($Version -replace '^(\d+\.\d+\.\d+).*$', '$1')
if ($msiVersion -notmatch '^\d+\.\d+\.\d+$') { $msiVersion = '0.0.0' }

# The license dialog needs RTF; derive it from LICENSE so the two never drift apart.
$root = Resolve-Path (Join-Path $PSScriptRoot '..\..')
$text = Get-Content (Join-Path $root 'LICENSE') -Raw
$escaped = $text.Replace('\', '\\').Replace('{', '\{').Replace('}', '\}') -replace "`r?`n", "\par`n"
$rtf = Join-Path ([IO.Path]::GetTempPath()) 'k8s-buddy-license.rtf'
"{\rtf1\ansi\deff0{\fonttbl{\f0 Consolas;}}\f0\fs16 $escaped}" | Set-Content -Path $rtf -Encoding ascii

foreach ($f in 'LICENSE', 'NOTICE') { Copy-Item (Join-Path $root $f) (Join-Path $BinDir $f) -Force }
New-Item -ItemType Directory -Force -Path (Split-Path $Out) | Out-Null

wix build (Join-Path $root 'packaging\windows\k8s-buddy.wxs') `
	-arch $Arch `
	-ext WixToolset.UI.wixext `
	-d "Version=$msiVersion" -d "BinDir=$BinDir" -d "LicenseRtf=$rtf" `
	-o $Out
if ($LASTEXITCODE -ne 0) { throw "wix build failed ($LASTEXITCODE)" }
Write-Host "built $Out (MSI version $msiVersion)"
