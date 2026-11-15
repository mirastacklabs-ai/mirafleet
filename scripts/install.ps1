<#
.SYNOPSIS
  Install the mirafleet binary from a GitHub release (Windows amd64/arm64, PowerShell 5.1 and 7).
.DESCRIPTION
  HTTPS only. The SHA-256 of the zip is always verified against checksums.txt. The cosign
  signature of checksums.txt is verified when cosign is on PATH, and required with
  -VerifySignature. Installs to %LocalAppData%\Programs\mirafleet\mirafleet.exe. Never needs
  admin. The user PATH is changed only with -AddToPath.

  A zip downloaded in a browser is marked as downloaded from the internet. After checking its
  checksum, run: Unblock-File .\mirafleet.exe  (or Properties -> Unblock, best done on the zip).
.EXAMPLE
  irm https://github.com/mirastacklabs-ai/mirafleet/releases/latest/download/install.ps1 | iex
.EXAMPLE
  .\install.ps1 -Version v0.6.0 -AddToPath -VerifySignature
#>
[CmdletBinding()]
param(
    [string]$Version = '',
    [string]$InstallDir = '',
    [switch]$AddToPath,
    [switch]$VerifySignature
)
$ErrorActionPreference = 'Stop'
try { [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12 } catch { }

$Repo = 'mirastacklabs-ai/mirafleet'
# Exact keyless identity: the release workflow of this repository, run for a tag.
$IdentityRe = '^https://github\.com/mirastacklabs-ai/mirafleet/\.github/workflows/release\.yml@refs/tags/v[0-9A-Za-z.+-]+$'
$OidcIssuer = 'https://token.actions.githubusercontent.com'

function Fail([string]$Msg) { throw "install.ps1: $Msg" }

function Get-File([string]$Url, [string]$Dest) {
    if (-not $Url.StartsWith('https://')) { Fail "refusing non-HTTPS URL: $Url" }
    try { Invoke-WebRequest -UseBasicParsing -Uri $Url -OutFile $Dest } catch { Fail "download failed: $Url ($($_.Exception.Message))" }
}

$base = $env:MIRAFLEET_INSTALL_BASE_URL
if ($base) {
    if (-not $base.StartsWith('https://')) { Fail "MIRAFLEET_INSTALL_BASE_URL must be https://, got: $base" }
    $base = $base.TrimEnd('/')
    if (-not $Version) { Fail 'MIRAFLEET_INSTALL_BASE_URL needs an explicit -Version' }
}

switch ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()) {
    'X64'   { $arch = 'amd64' }
    'Arm64' { $arch = 'arm64' }
    default { Fail "unsupported architecture $([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture)" }
}

if (-not $Version) {
    $req = [System.Net.HttpWebRequest]::Create("https://github.com/$Repo/releases/latest")
    $req.AllowAutoRedirect = $false
    try { $resp = $req.GetResponse() } catch [System.Net.WebException] { $resp = $_.Exception.Response }
    if (-not $resp -or -not $resp.Headers['Location']) { Fail 'cannot resolve the latest release' }
    $Version = ($resp.Headers['Location'] -split '/')[-1]
    $resp.Close()
}
if ($Version -notmatch '^v\d+\.\d+\.\d+([-+.][0-9A-Za-z.-]+)?$') { Fail "invalid version: $Version" }
if (-not $base) { $base = "https://github.com/$Repo/releases/download/$Version" }

if (-not $InstallDir) { $InstallDir = Join-Path $env:LocalAppData 'Programs\mirafleet' }
$archive = "mirafleet_$($Version.Substring(1))_windows_$arch.zip"

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ('mirafleet-install-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Write-Host "Installing mirafleet $Version (windows/$arch) to $InstallDir"
    Get-File "$base/checksums.txt" "$tmp\checksums.txt"

    $cosign = Get-Command cosign -ErrorAction SilentlyContinue
    if ($VerifySignature -and -not $cosign) { Fail '-VerifySignature needs cosign on PATH' }
    if ($cosign) {
        Get-File "$base/checksums.txt.sig" "$tmp\checksums.txt.sig"
        Get-File "$base/checksums.txt.pem" "$tmp\checksums.txt.pem"
        & $cosign.Source verify-blob --certificate "$tmp\checksums.txt.pem" --signature "$tmp\checksums.txt.sig" `
            --certificate-identity-regexp $IdentityRe --certificate-oidc-issuer $OidcIssuer "$tmp\checksums.txt" *> $null
        if ($LASTEXITCODE -ne 0) { Fail 'cosign signature verification of checksums.txt FAILED' }
        Write-Host 'cosign signature of checksums.txt verified'
    } else {
        Write-Host 'cosign not found: skipping signature check (checksum is still verified)'
    }

    $want = $null
    foreach ($line in Get-Content "$tmp\checksums.txt") {
        $f = $line -split '\s+', 2
        if ($f.Count -eq 2 -and ($f[1].TrimStart('*') -ceq $archive)) { $want = $f[0]; break }
    }
    if (-not $want) { Fail "$archive is not listed in checksums.txt" }
    if ($want -notmatch '^[0-9a-fA-F]{64}$') { Fail "malformed checksum for $archive" }

    Get-File "$base/$archive" "$tmp\$archive"
    $got = (Get-FileHash -Algorithm SHA256 -Path "$tmp\$archive").Hash
    if ($got -ne $want) { Fail "checksum mismatch for $archive (want $want, got $got)" }
    Write-Host 'sha256 verified'

    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $zip = [System.IO.Compression.ZipFile]::OpenRead("$tmp\$archive")
    try {
        $entry = $zip.Entries | Where-Object { $_.FullName -ceq 'mirafleet.exe' } | Select-Object -First 1
        if (-not $entry) { Fail "$archive does not contain mirafleet.exe" }
        New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
        [System.IO.Compression.ZipFileExtensions]::ExtractToFile($entry, (Join-Path $InstallDir 'mirafleet.exe'), $true)
    } finally { $zip.Dispose() }
    Write-Host "Installed $(Join-Path $InstallDir 'mirafleet.exe')"

    if ($AddToPath) {
        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        $parts = @(); if ($userPath) { $parts = $userPath -split ';' | Where-Object { $_ } }
        if ($parts -notcontains $InstallDir) {
            [Environment]::SetEnvironmentVariable('Path', (($parts + $InstallDir) -join ';'), 'User')
            Write-Host 'Added to your user PATH; open a new terminal.'
        }
    } else {
        Write-Host "Note: $InstallDir is not added to PATH (use -AddToPath)."
    }
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
