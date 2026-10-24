# Verify that packaging stops at the first failed native command.
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path $PSScriptRoot -Parent
$scratch = Join-Path $projectRoot ('.build-test-' + [guid]::NewGuid().ToString('N'))
$savedPath = $env:PATH
$savedLog = $env:GOTM_TEST_GO_LOG
$savedLocation = Get-Location
$savedBuildEnv = @{}
foreach ($name in @('GOOS','GOARCH','CGO_ENABLED')) { $savedBuildEnv[$name] = [Environment]::GetEnvironmentVariable($name,'Process') }
New-Item -ItemType Directory -Path $scratch | Out-Null
try {
    Copy-Item -LiteralPath (Join-Path $projectRoot 'build.ps1') -Destination (Join-Path $scratch 'build.ps1')
    $env:GOTM_TEST_GO_LOG = Join-Path $scratch 'calls.txt'
    Set-Content -LiteralPath (Join-Path $scratch 'go.cmd') -Encoding ascii -Value @('@echo off','echo invoked>>"%GOTM_TEST_GO_LOG%"','exit /b 7')
    $env:PATH = "$scratch;$savedPath"
    $failed = $false
    try { & (Join-Path $scratch 'build.ps1') -Version '0.0.0' } catch { $failed = $true }
    $calls = @(Get-Content -LiteralPath $env:GOTM_TEST_GO_LOG)
    if (-not $failed -or $calls.Count -ne 1) {
        throw "Build continued after a failed Go command ($($calls.Count) commands ran)."
    }
    if (Test-Path -LiteralPath (Join-Path $scratch 'dist')) { throw 'A failed build started packaging.' }
    if ((Get-Location).Path -ne $savedLocation.Path) { throw 'Build did not restore the working directory.' }
    foreach ($name in $savedBuildEnv.Keys) {
        if ([Environment]::GetEnvironmentVariable($name,'Process') -ne $savedBuildEnv[$name]) { throw "Build changed $name after failure." }
    }
    Write-Host 'PASS: first native failure stops packaging and restores location/environment.'

    # -TrustedSigning with an unfilled signing.json must fail before any Go command runs.
    Set-Content -LiteralPath $env:GOTM_TEST_GO_LOG -Value @() -Encoding ascii
    $cfg = Join-Path $scratch 'signing.json'
    Copy-Item -LiteralPath (Join-Path $projectRoot 'signing.json') -Destination $cfg
    $signFailed = $null
    try { & (Join-Path $scratch 'build.ps1') -Version '0.0.0' -TrustedSigning -SigningConfig $cfg } catch { $signFailed = $_.Exception.Message }
    $calls = @(Get-Content -LiteralPath $env:GOTM_TEST_GO_LOG | Where-Object { $_ -ne '' })
    if (-not $signFailed -or $signFailed -notmatch 'not filled in' -or $calls.Count -ne 0) {
        throw "An unfilled signing.json did not stop the build first (error: '$signFailed', $($calls.Count) Go commands ran)."
    }
    Write-Host 'PASS: -TrustedSigning refuses an unfilled signing.json before building.'
} finally {
	Set-Location -LiteralPath $savedLocation.Path
    foreach ($name in $savedBuildEnv.Keys) {
        if ($null -eq $savedBuildEnv[$name]) { Remove-Item -LiteralPath "Env:\$name" -ErrorAction SilentlyContinue }
        else { Set-Item -LiteralPath "Env:\$name" -Value $savedBuildEnv[$name] }
    }
    $env:PATH = $savedPath
    $env:GOTM_TEST_GO_LOG = $savedLog
    $resolvedScratch = [IO.Path]::GetFullPath($scratch)
    if (-not $resolvedScratch.StartsWith($projectRoot + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) { throw 'Cleanup escaped the project directory.' }
    Remove-Item -LiteralPath $resolvedScratch -Recurse -Force
}
