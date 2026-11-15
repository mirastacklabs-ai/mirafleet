<#
.SYNOPSIS
  Tests scripts/install.ps1 against a local HTTPS server serving fake release files.
.DESCRIPTION
  Runs on Windows (CI, windows-latest) and in a PowerShell container on Linux:
    docker run --rm -v "$PWD/scripts":/scripts:ro <pinned mcr.microsoft.com/powershell image> \
      pwsh -NoProfile -File /scripts/release/installtest.ps1
  The cosign cases use a stand-in cosign that accepts only the exact identity and issuer.
  The server is in-process C# (TcpListener + SslStream) with a throwaway self-signed certificate.
#>
$ErrorActionPreference = 'Stop'
$Install = Join-Path $PSScriptRoot '..\install.ps1'
$W = Join-Path ([System.IO.Path]::GetTempPath()) ('installtest-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path "$W\www", "$W\bin" | Out-Null
$script:fail = 0
function Ok([string]$m) { Write-Host "ok   $m" }
function Bad([string]$m) { Write-Host "FAIL $m"; $script:fail = 1 }

Add-Type -TypeDefinition @'
using System; using System.IO; using System.Net; using System.Net.Sockets; using System.Net.Security;
using System.Security.Cryptography; using System.Security.Cryptography.X509Certificates; using System.Text; using System.Threading;
public static class TestServer {
  public static string CertPem; public static int Port; static X509Certificate2 cert; static TcpListener l; static string root;
  public static X509Certificate2 Start(string dir) {
    root = dir;
    using (var rsa = RSA.Create(2048)) {
      var req = new CertificateRequest("CN=localhost", rsa, HashAlgorithmName.SHA256, RSASignaturePadding.Pkcs1);
      var san = new SubjectAlternativeNameBuilder(); san.AddDnsName("localhost"); req.CertificateExtensions.Add(san.Build());
      var c = req.CreateSelfSigned(DateTimeOffset.UtcNow.AddMinutes(-5), DateTimeOffset.UtcNow.AddDays(1));
      cert = new X509Certificate2(c.Export(X509ContentType.Pfx, "x"), "x", X509KeyStorageFlags.Exportable);
      CertPem = "-----BEGIN CERTIFICATE-----\n" + Convert.ToBase64String(c.RawData, Base64FormattingOptions.InsertLineBreaks) + "\n-----END CERTIFICATE-----\n";
    }
    l = new TcpListener(IPAddress.Loopback, 0); l.Start(); Port = ((IPEndPoint)l.LocalEndpoint).Port;
    new Thread(Loop) { IsBackground = true }.Start();
    return cert;
  }
  static void Loop() { while (true) { var c = l.AcceptTcpClient(); new Thread(() => Serve(c)) { IsBackground = true }.Start(); } }
  static void Serve(TcpClient c) {
    try {
      using (c) using (var s = new SslStream(c.GetStream(), false)) {
        s.AuthenticateAsServer(cert, false, System.Security.Authentication.SslProtocols.Tls12, false);
        var sb = new StringBuilder(); int b;
        while (!sb.ToString().EndsWith("\r\n\r\n") && (b = s.ReadByte()) >= 0) sb.Append((char)b);
        var parts = sb.ToString().Split(' '); var name = Path.GetFileName(parts[1].Split('?')[0]);
        var p = Path.Combine(root, name); byte[] body; string status;
        if (name.Length > 0 && File.Exists(p)) { body = File.ReadAllBytes(p); status = "200 OK"; } else { body = new byte[0]; status = "404 Not Found"; }
        var h = Encoding.ASCII.GetBytes("HTTP/1.1 " + status + "\r\nContent-Length: " + body.Length + "\r\nConnection: close\r\n\r\n");
        s.Write(h, 0, h.Length); s.Write(body, 0, body.Length); s.Flush();
      }
    } catch { }
  }
}
'@
$cert = [TestServer]::Start("$W\www")
$certFile = "$W\cert.pem"
[IO.File]::WriteAllText($certFile, [TestServer]::CertPem)
$isWin = ($PSVersionTable.PSEdition -eq 'Desktop') -or $IsWindows
if ($isWin) {
    $pub = New-Object System.Security.Cryptography.X509Certificates.X509Certificate2 -ArgumentList (, $cert.RawData)
    $store = New-Object System.Security.Cryptography.X509Certificates.X509Store 'Root', 'CurrentUser'
    $store.Open('ReadWrite'); $store.Add($pub); $store.Close()
} else {
    $env:SSL_CERT_FILE = $certFile
}
$env:MIRAFLEET_INSTALL_BASE_URL = "https://localhost:$([TestServer]::Port)"

$arch = if ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture -eq 'Arm64') { 'arm64' } else { 'amd64' }
$V = 'v9.9.9'
$archive = "mirafleet_9.9.9_windows_$arch.zip"
Set-Content -NoNewline -Path "$W\mirafleet.exe" -Value 'fake'
Add-Type -AssemblyName System.IO.Compression.FileSystem
New-Item -ItemType Directory -Path "$W\stage2" | Out-Null
Copy-Item "$W\mirafleet.exe" "$W\stage2\mirafleet.exe"
[System.IO.Compression.ZipFile]::CreateFromDirectory("$W\stage2", "$W\www\$archive")
$good = (Get-FileHash -Algorithm SHA256 "$W\www\$archive").Hash.ToLower()
function Set-Sums([string]$h, [string]$name) { Set-Content -Path "$W\www\checksums.txt" -Value "$h  $name" }
Set-Sums $good $archive
$goodIdent = 'https://github.com/mirastacklabs-ai/mirafleet/.github/workflows/release.yml@refs/tags/v9.9.9'
Set-Content -Path "$W\www\checksums.txt.sigstore.json" -Value "sig-by: $goodIdent"

# Run install.ps1 in a child shell so a thrown failure becomes an exit code; returns @(code, output).
$shell = (Get-Process -Id $PID).Path
function Run-Install([string]$dir, [string[]]$extra = @()) {
    $out = & $shell -NoProfile -ExecutionPolicy Bypass -File $Install -Version $V -InstallDir $dir @extra 2>&1 | Out-String
    return @($LASTEXITCODE, $out)
}
function Exe([string]$dir) { Test-Path (Join-Path $dir 'mirafleet.exe') }

$r = Run-Install "$W\d1"
if ($r[0] -eq 0 -and (Exe "$W\d1")) { Ok 'good checksum installs' } else { Bad 'good checksum'; Write-Host $r[1] }

Set-Sums ('0' * 64) $archive
$r = Run-Install "$W\d2"
if ($r[0] -ne 0 -and -not (Exe "$W\d2") -and $r[1] -match 'mismatch') { Ok 'bad checksum refused' } else { Bad 'bad checksum'; Write-Host $r[1] }

Set-Sums $good 'other.zip'
$r = Run-Install "$W\d3"
if ($r[0] -ne 0 -and -not (Exe "$W\d3") -and $r[1] -match 'not listed') { Ok 'unlisted archive refused' } else { Bad 'unlisted'; Write-Host $r[1] }
Set-Sums $good $archive

$env:MIRAFLEET_INSTALL_BASE_URL = 'http://localhost:1'
$r = Run-Install "$W\d4"
if ($r[0] -ne 0 -and -not (Exe "$W\d4") -and $r[1] -match 'https') { Ok 'http refused' } else { Bad 'http'; Write-Host $r[1] }
$env:MIRAFLEET_INSTALL_BASE_URL = "https://localhost:$([TestServer]::Port)"

$r = Run-Install "$W\d5" @('-VerifySignature')
if ($r[0] -ne 0 -and -not (Exe "$W\d5") -and $r[1] -match 'cosign') { Ok '-VerifySignature without cosign refused' } else { Bad 'no cosign'; Write-Host $r[1] }

# Stand-in cosign: a PowerShell-backed launcher that accepts only the exact identity regexp and issuer.
$stub = "$W\bin\cosign.ps1"
Set-Content -Path $stub -Value @'
param([Parameter(ValueFromRemainingArguments=$true)]$a)
$bi = [array]::IndexOf($a, '--bundle')
if ($bi -lt 0) { exit 1 }
$sig = $a[$bi + 1]
# must be given the published bundle name (what goreleaser uploads)
if ((Split-Path -Leaf $sig) -ne 'checksums.txt.sigstore.json') { exit 1 }
$re = $a[[array]::IndexOf($a, '--certificate-identity-regexp') + 1]
$iss = $a[[array]::IndexOf($a, '--certificate-oidc-issuer') + 1]
if ($iss -ne 'https://token.actions.githubusercontent.com') { exit 1 }
$ident = ((Get-Content $sig -Raw) -replace '^sig-by:\s*', '').Trim()
if ($ident -match $re) { exit 0 } else { exit 1 }
'@
if ($isWin) {
    Set-Content -Path "$W\bin\cosign.cmd" -Value "@echo off`r`npowershell -NoProfile -ExecutionPolicy Bypass -File `"$stub`" %*"
} else {
    Set-Content -Path "$W\bin\cosign" -Value "#!/bin/sh`nexec pwsh -NoProfile -File '$stub' `"`$@`""
    chmod +x "$W\bin\cosign"
}
$sep = [IO.Path]::PathSeparator
$oldPath = $env:PATH
$env:PATH = "$W\bin$sep$env:PATH"
$r = Run-Install "$W\d6"
if ($r[0] -eq 0 -and (Exe "$W\d6")) { Ok 'cosign present, right identity installs' } else { Bad 'right identity'; Write-Host $r[1] }
Set-Content -Path "$W\www\checksums.txt.sigstore.json" -Value 'sig-by: https://github.com/evil/mirafleet/.github/workflows/release.yml@refs/tags/v9.9.9'
$r = Run-Install "$W\d7"
if ($r[0] -ne 0 -and -not (Exe "$W\d7") -and $r[1] -match 'FAILED') { Ok 'wrong cosign identity refused' } else { Bad 'wrong identity'; Write-Host $r[1] }
$env:PATH = $oldPath

# PATH untouched without -AddToPath is checked by reading the user PATH on Windows only.
Remove-Item -Recurse -Force $W -ErrorAction SilentlyContinue
if ($script:fail -eq 0) { Write-Host 'installtest.ps1: OK' } else { Write-Host 'installtest.ps1: FAIL'; exit 1 }
