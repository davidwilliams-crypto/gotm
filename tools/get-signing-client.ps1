<#
.SYNOPSIS
  Fetches Microsoft's Trusted Signing client (the signtool plug-in) and
  verifies it before anything uses it.

.DESCRIPTION
  Downloads the Microsoft.Trusted.Signing.Client NuGet package once into
  tools\signing\ and checks that Azure.CodeSigning.Dlib.dll carries a valid
  Authenticode signature from Microsoft Corporation. Prints the path of the
  verified DLL. build.ps1 -TrustedSigning calls this; it can also be run on
  its own to pre-fetch the client on a build PC.
#>
param(
  [string]$Version = '1.0.95',
  [string]$CacheDir = "$PSScriptRoot\signing"
)
$ErrorActionPreference = 'Stop'
$pkg = "Microsoft.Trusted.Signing.Client.$Version"
$dir = Join-Path $CacheDir $pkg
$dlib = Join-Path $dir 'bin\x64\Azure.CodeSigning.Dlib.dll'

function Test-MicrosoftSigned([string]$path) {
  $sig = Get-AuthenticodeSignature -LiteralPath $path
  if ($sig.Status -ne 'Valid') { throw "$path is not validly signed ($($sig.Status))." }
  if ($sig.SignerCertificate.Subject -notmatch 'O=Microsoft Corporation') {
    throw "$path is signed by '$($sig.SignerCertificate.Subject)', not Microsoft Corporation."
  }
}

if (-not (Test-Path -LiteralPath $dlib)) {
  New-Item -ItemType Directory -Force -Path $CacheDir | Out-Null
  $tmp = Join-Path $CacheDir "$pkg.zip"
  $url = "https://www.nuget.org/api/v2/package/Microsoft.Trusted.Signing.Client/$Version"
  Write-Host "downloading $pkg from nuget.org"
  Invoke-WebRequest -Uri $url -OutFile $tmp -UseBasicParsing
  if (Test-Path -LiteralPath $dir) { Remove-Item -LiteralPath $dir -Recurse -Force }
  Expand-Archive -LiteralPath $tmp -DestinationPath $dir -Force
  Remove-Item -LiteralPath $tmp -Force
  if (-not (Test-Path -LiteralPath $dlib)) { throw "the package did not contain bin\x64\Azure.CodeSigning.Dlib.dll" }
}
# Verified on every use, not just after the download: the cache is a plain folder.
Test-MicrosoftSigned $dlib
Get-ChildItem -LiteralPath (Split-Path $dlib) -Filter 'Azure.CodeSigning*.dll' | ForEach-Object { Test-MicrosoftSigned $_.FullName }
Write-Output $dlib
