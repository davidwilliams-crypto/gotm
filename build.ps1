<#
  Build, test and package gotm (native Win32 app, no cgo).

    .\build.ps1 -Version 1.2.3            # full release kit in dist\release\gotm-1.2.3\
    .\build.ps1 -Version 1.2.3 -SkipTests
    .\build.ps1 -Version 1.2.3 -TrustedSigning              # sign both executables with Azure Trusted Signing (signing.json)
    .\build.ps1 -Version 1.2.3 -SignThumbprint <cert SHA-1>   # sign with a certificate from the Windows certificate store

  The release kit contains:
    gotm-<v>-setup.exe        installer/uninstaller (all users, Program Files, Start menu, Apps & features)
    gotm-<v>-portable.zip     gotm.exe + README.txt + THIRD-PARTY-NOTICES.txt
    README.txt                end-user instructions
    SHA256SUMS.txt            checksums of the files above

  Nothing outside Go is needed. Signing is optional and needs signtool.exe (Windows SDK).
  -TrustedSigning reads the account settings from signing.json (see docs\SIGNING.md), fetches
  Microsoft's signing client once (tools\get-signing-client.ps1, verified Microsoft-signed), and
  signs with the Azure credentials of the signed-in user (Azure CLI, Azure PowerShell or the
  AZURE_* environment variables). Every signed file is verified with signtool afterwards.
#>
param(
  [ValidatePattern('^\d+\.\d+\.\d+$')]
  [string]$Version = "0.9.4",
  [switch]$SkipTests,
  [string]$SignThumbprint = "",
  [switch]$TrustedSigning,
  [string]$SigningConfig = "signing.json"
)
$ErrorActionPreference = "Stop"
foreach ($part in $Version.Split('.')) {
    if ([uint64]$part -gt 65535) { throw 'Version components must be between 0 and 65535.' }
}
if ($TrustedSigning -and $SignThumbprint -ne '') { throw 'Use either -TrustedSigning or -SignThumbprint, not both.' }

# Trusted Signing settings are checked before anything is built, so a wrong
# signing.json fails in a second rather than after the tests.
$signing = $null
if ($TrustedSigning) {
    $cfgPath = if ([IO.Path]::IsPathRooted($SigningConfig)) { $SigningConfig } else { Join-Path $PSScriptRoot $SigningConfig }
    if (-not (Test-Path -LiteralPath $cfgPath)) { throw "-TrustedSigning needs $cfgPath (see docs\SIGNING.md)." }
    $signing = Get-Content -LiteralPath $cfgPath -Raw | ConvertFrom-Json
    foreach ($key in @('endpoint','account','profile')) {
        $value = [string]$signing.$key
        if ($value -eq '' -or $value -like 'FILL-IN*') { throw "${cfgPath}: '$key' is not filled in (see docs\SIGNING.md)." }
    }
    if ([string]$signing.endpoint -notmatch '^https://[a-z0-9-]+\.codesigning\.azure\.net/?$') {
        throw "${cfgPath}: 'endpoint' should look like https://eus.codesigning.azure.net"
    }
}

function Invoke-Native {
    param([string]$Command, [string[]]$Arguments)
    & $Command @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "$Command failed with exit code ${LASTEXITCODE}: $($Arguments -join ' ')"
    }
}

function Get-SignTool {
    $signtool = Get-Command 'signtool.exe' -ErrorAction SilentlyContinue
    if (-not $signtool) {
        $signtool = Get-ChildItem "${env:ProgramFiles(x86)}\Windows Kits\10\bin\*\x64\signtool.exe" -ErrorAction SilentlyContinue |
            Sort-Object FullName -Descending | Select-Object -First 1
    }
    if (-not $signtool) { throw 'Signing was requested but signtool.exe was not found (install the Windows SDK).' }
    if ($signtool.Source) { $signtool.Source } else { $signtool.FullName }
}

$trustedDlib = $null
$trustedMetadata = $null
if ($TrustedSigning) {
    $trustedDlib = & (Join-Path $PSScriptRoot 'tools\get-signing-client.ps1')
    # What the plug-in needs to know: where the account lives and which profile
    # signs. Credentials are never in here; the plug-in takes them from the
    # signed-in Azure session (or AZURE_* variables). Cloud-only credential
    # sources are skipped so a build PC does not wait on them.
    $exclude = @('ManagedIdentityCredential','WorkloadIdentityCredential')
    if ($signing.excludeCredentials) { $exclude = @($signing.excludeCredentials) }
    $trustedMetadata = Join-Path ([IO.Path]::GetTempPath()) "gotm-signing-$PID.json"
    @{
        Endpoint               = [string]$signing.endpoint
        CodeSigningAccountName = [string]$signing.account
        CertificateProfileName = [string]$signing.profile
        ExcludeCredentials     = $exclude
    } | ConvertTo-Json | Set-Content -LiteralPath $trustedMetadata -Encoding ascii
}

function Invoke-Sign {
    param([string]$Path)
    if (-not $TrustedSigning -and $SignThumbprint -eq '') { return }
    $tool = Get-SignTool
    if ($TrustedSigning) {
        Invoke-Native $tool @('sign','/v','/fd','SHA256','/tr','http://timestamp.acs.microsoft.com','/td','SHA256',
            '/dlib',$trustedDlib,'/dmdf',$trustedMetadata,$Path)
    } else {
        Invoke-Native $tool @('sign','/sha1',$SignThumbprint,'/fd','SHA256','/tr','http://timestamp.digicert.com','/td','SHA256',$Path)
    }
    # A signature that Windows would not accept must fail the build, not ship.
    Invoke-Native $tool @('verify','/pa',$Path)
    Write-Host "signed and verified $Path"
}

$payload = 'tools\setup\payload'
$setupSyso = 'tools\setup\rsrc_windows_amd64.syso'
$savedEnv = @{}
foreach ($name in @('GOOS','GOARCH','CGO_ENABLED')) { $savedEnv[$name] = [Environment]::GetEnvironmentVariable($name,'Process') }
Push-Location -LiteralPath $PSScriptRoot
try {
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    $env:CGO_ENABLED = '0'

    # Common-Controls v6 and DPI manifest, icon, and matching version metadata.
    Invoke-Native 'go' @('run','./tools/genicon')
    Invoke-Native 'go' @('run','github.com/tc-hib/go-winres@v0.3.3','make','--arch','amd64','--product-version',$Version,'--file-version',$Version)
    Invoke-Native 'go' @('vet','./...')
    if (-not $SkipTests) { Invoke-Native 'go' @('test','-count=1','./...') }

    New-Item -ItemType Directory -Force -Path 'dist' | Out-Null
    Invoke-Native 'go' @('build','-trimpath','-ldflags',"-s -w -H=windowsgui -X main.version=$Version",'-o','dist\gotm.exe','.')
    Invoke-Sign 'dist\gotm.exe'
    Write-Host "built dist\gotm.exe ($Version)"

    Invoke-Native 'go' @('run','./tools/notices','-o','dist\THIRD-PARTY-NOTICES.txt','.')
    Copy-Item -LiteralPath 'docs\README-user.txt' -Destination 'dist\README.txt' -Force

    # The installer embeds gotm.exe, the README and the notices, zipped.
    Compress-Archive -Path 'dist\gotm.exe','dist\README.txt','dist\THIRD-PARTY-NOTICES.txt' -DestinationPath "$payload\payload.zip" -Force
    # Its own manifest asks for administrator rights (it writes to Program Files and HKLM).
    # Generated only now: the test binary for tools/setup must not carry it.
    Invoke-Native 'go' @('run','github.com/tc-hib/go-winres@v0.3.3','make','--in','tools\setup\winres\winres.json','--out','tools\setup\rsrc','--arch','amd64','--product-version',$Version,'--file-version',$Version)
    $setup = "dist\gotm-$Version-setup.exe"
    Invoke-Native 'go' @('build','-trimpath','-ldflags',"-s -w -H=windowsgui -X main.version=$Version",'-o',$setup,'./tools/setup')
    Invoke-Sign $setup
    Write-Host "built $setup"

    $rel = "dist\release\gotm-$Version"
    if (Test-Path -LiteralPath $rel) { Remove-Item -LiteralPath $rel -Recurse -Force }
    New-Item -ItemType Directory -Force -Path $rel | Out-Null
    $zip = "$rel\gotm-$Version-portable.zip"
    Compress-Archive -Path 'dist\gotm.exe','dist\README.txt','dist\THIRD-PARTY-NOTICES.txt' -DestinationPath $zip -Force
    Copy-Item -LiteralPath $setup -Destination $rel
    Copy-Item -LiteralPath 'dist\README.txt' -Destination $rel
    $sums = Get-ChildItem -LiteralPath $rel -File | Where-Object Name -ne 'SHA256SUMS.txt' | Sort-Object Name | ForEach-Object {
        "{0}  {1}" -f (Get-FileHash -Algorithm SHA256 -LiteralPath $_.FullName).Hash.ToLower(), $_.Name
    }
    Set-Content -LiteralPath "$rel\SHA256SUMS.txt" -Value $sums -Encoding ascii
    Write-Host "release kit: $rel"
    Get-ChildItem -LiteralPath $rel | ForEach-Object { Write-Host ("  {0,-34} {1,10:N0} bytes" -f $_.Name, $_.Length) }
} finally {
    Remove-Item -LiteralPath "$payload\payload.zip" -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $setupSyso -ErrorAction SilentlyContinue
    if ($trustedMetadata) { Remove-Item -LiteralPath $trustedMetadata -ErrorAction SilentlyContinue }
    foreach ($name in $savedEnv.Keys) {
        if ($null -eq $savedEnv[$name]) { Remove-Item -LiteralPath "Env:\$name" -ErrorAction SilentlyContinue }
        else { Set-Item -LiteralPath "Env:\$name" -Value $savedEnv[$name] }
    }
    Pop-Location
}
