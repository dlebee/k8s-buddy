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
# Pinned hashes: WiX builds the MSI before it is signed, so a tampered toolchain would get signed too.
$pinned = @{
	"wix.$wixVersion.nupkg"                  = 'F30EF0C74E2A986126539C5780BE93AC24E8136EAF723B1937B26272703AE173'
	"wixtoolset.ui.wixext.$wixVersion.nupkg" = '5EF2C707614B9F70B6BBADD2D4ABCB4124EFEE215E9B16BFBC80113079A604C7'
}
$feed = Join-Path ([IO.Path]::GetTempPath()) 'k8s-buddy-wix-feed'
New-Item -ItemType Directory -Force -Path $feed | Out-Null
foreach ($file in $pinned.Keys) {
	$path = Join-Path $feed $file
	if (-not (Test-Path $path)) {
		$id = $file -replace "\.$([regex]::Escape($wixVersion))\.nupkg$", ''
		Invoke-WebRequest "https://api.nuget.org/v3-flatcontainer/$id/$wixVersion/$file" -OutFile $path
	}
	if ((Get-FileHash $path -Algorithm SHA256).Hash -ne $pinned[$file]) { throw "hash mismatch for $file" }
}

if (-not (Get-Command wix -ErrorAction SilentlyContinue)) {
	# Install only from the verified local feed.
	$config = Join-Path $feed 'nuget.config'
	"<configuration><packageSources><clear /><add key=`"local`" value=`"$feed`" /></packageSources></configuration>" | Set-Content $config
	dotnet tool install --global wix --version $wixVersion --configfile $config | Out-Host
	if ($LASTEXITCODE -ne 0) { throw 'wix install failed' }
	$env:PATH += ";$env:USERPROFILE\.dotnet\tools"
}
$uiExt = Join-Path $feed 'ui-ext'
Expand-Archive (Join-Path $feed "wixtoolset.ui.wixext.$wixVersion.nupkg") -DestinationPath $uiExt -Force
$uiDll = Get-ChildItem $uiExt -Filter WixToolset.UI.wixext.dll -Recurse | Select-Object -First 1 -ExpandProperty FullName
if (-not $uiDll) { throw 'WixToolset.UI.wixext.dll not found' }

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
	-ext $uiDll `
	-d "Version=$msiVersion" -d "BinDir=$BinDir" -d "LicenseRtf=$rtf" -d "IconFile=$(Join-Path $root 'packaging\windows\k8s-buddy.ico')" `
	-o $Out
if ($LASTEXITCODE -ne 0) { throw "wix build failed ($LASTEXITCODE)" }
# WiX writes a debug database next to the MSI; it isn't something to ship.
Remove-Item -Force -ErrorAction SilentlyContinue ([IO.Path]::ChangeExtension($Out, '.wixpdb'))
Write-Host "built $Out (MSI version $msiVersion)"
