<#
.SYNOPSIS
  Runs gotm's elevated self-tests through one UAC prompt and saves the log.

.DESCRIPTION
  Compiles the metrics test binary, then starts an elevated PowerShell that
  runs the tests tagged "Elevated" (service control, ending a SYSTEM process,
  the AMD Ryzen Monitoring SDK) and, when gotm.exe has been built, opens the
  elevated app on the Power & Freq page for a few seconds and captures its
  window. Everything the elevated part changes (a temporary service, the
  machine-wide AMD SDK switch) is removed or restored before it exits.

  Output: <OutDir>\elevated-tests.txt and elevated-power.png
#>
param(
  [string]$OutDir = "$PSScriptRoot\..\dist\elevated-check",
  [switch]$NoApp
)
$ErrorActionPreference = 'Stop'
$root = Resolve-Path "$PSScriptRoot\.."
New-Item -ItemType Directory -Force $OutDir | Out-Null
$OutDir = (Resolve-Path $OutDir).Path
$testExe = Join-Path $OutDir 'metrics.test.exe'

Push-Location $root
try {
  & go test -c -o $testExe ./internal/metrics
  if ($LASTEXITCODE -ne 0) { throw 'compiling the test binary failed' }
} finally { Pop-Location }

$inner = @'
param($TestExe, $OutDir, $AppExe)
$log = Join-Path $OutDir 'elevated-tests.txt'
# quoted so PowerShell hands "-test.run=Elevated" to the binary whole (unquoted, it splits at the dot)
& $TestExe '-test.run=Elevated' '-test.v' '-test.timeout=5m' 2>&1 | Out-File -Encoding utf8 $log
"exit code: $LASTEXITCODE" | Out-File -Encoding utf8 -Append $log
# the SDK test again under each condition the app's process adds
foreach ($mode in 'dlldirs','comsec','both') {
  "== SDK test with GOTM_SDK_EMULATE=$mode ==" | Out-File -Encoding utf8 -Append $log
  $env:GOTM_SDK_EMULATE = $mode
  & $TestExe '-test.run=ElevatedRyzenSDK' '-test.v' '-test.timeout=2m' 2>&1 | Out-File -Encoding utf8 -Append $log
  Remove-Item Env:GOTM_SDK_EMULATE -ErrorAction SilentlyContinue
}
# the SDK inside the real app process
if ($AppExe -and (Test-Path $AppExe)) {
  $probe = Join-Path $OutDir 'ryzen-probe.txt'
  Remove-Item $probe -ErrorAction SilentlyContinue
  $pp = Start-Process -FilePath $AppExe -ArgumentList '--ryzen-probe', $probe -PassThru
  if (-not $pp.WaitForExit(90000)) { Stop-Process -Id $pp.Id -Force; "probe still running after 90 s, stopped" | Out-File -Encoding utf8 -Append $log }
  "== gotm.exe --ryzen-probe (exit $($pp.ExitCode)) ==" | Out-File -Encoding utf8 -Append $log
  if (Test-Path $probe) { Get-Content $probe | Out-File -Encoding utf8 -Append $log } else { "no probe output written" | Out-File -Encoding utf8 -Append $log }
}
# AMD's own sample program is the control for the SDK: if it cannot read the
# CPU either, the limitation is the SDK's, not gotm's.
$sdk = (Get-ItemProperty 'HKLM:\SOFTWARE\AMD\RyzenMasterMonitoringSDK' -ErrorAction SilentlyContinue).InstallationPath
if ($sdk) {
  $sample = Join-Path $sdk 'RyzenMasterMonitoringSampleApp\bin-prebuilt\AMDRyzenMasterMonitoringSampleApp.exe'
  if (Test-Path $sample) {
    "== AMD sample app ($sample) ==" | Out-File -Encoding utf8 -Append $log
    $sout = Join-Path $OutDir 'amd-sample.txt'
    $sp = Start-Process -FilePath $sample -WorkingDirectory (Split-Path $sample) -RedirectStandardOutput $sout -PassThru -WindowStyle Hidden
    if (-not $sp.WaitForExit(30000)) { Stop-Process -Id $sp.Id -Force; "sample app still running after 30 s, stopped" | Out-File -Encoding utf8 -Append $log }
    "sample exit code: $($sp.ExitCode)" | Out-File -Encoding utf8 -Append $log
    if (Test-Path $sout) { Get-Content $sout | Select-Object -First 40 | Out-File -Encoding utf8 -Append $log }
  }
}
if ($AppExe -and (Test-Path $AppExe)) {
  Add-Type -AssemblyName System.Drawing
  Add-Type -ReferencedAssemblies System.Drawing @"
using System; using System.Runtime.InteropServices; using System.Drawing;
public class ShotE {
  [DllImport("user32.dll")] public static extern bool SetProcessDPIAware();
  [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] public static extern bool PrintWindow(IntPtr h, IntPtr hdc, uint flags);
  [StructLayout(LayoutKind.Sequential)] public struct RECT { public int L,T,R,B; }
  public static void Shot(IntPtr h, string path) {
    SetProcessDPIAware();
    RECT r; GetWindowRect(h, out r);
    using (Bitmap b = new Bitmap(r.R - r.L, r.B - r.T)) {
      using (Graphics g = Graphics.FromImage(b)) { IntPtr dc = g.GetHdc(); PrintWindow(h, dc, 2); g.ReleaseHdc(dc); }
      b.Save(path, System.Drawing.Imaging.ImageFormat.Png);
    }
  }
}
"@
  # turn the machine-wide AMD SDK switch on for the capture, then put it back
  $key = 'HKLM:\SOFTWARE\gotm'
  $had = $null
  try { $had = (Get-ItemProperty $key -Name RyzenSDK -ErrorAction Stop).RyzenSDK } catch {}
  New-Item -Path $key -Force | Out-Null
  Set-ItemProperty $key -Name RyzenSDK -Value 1 -Type DWord
  try {
    $p = Start-Process -FilePath $AppExe -ArgumentList '--tab','power' -PassThru
    Start-Sleep 15
    $p.Refresh()
    if (-not $p.HasExited) {
      "elevated gotm running, window handle $($p.MainWindowHandle)" | Out-File -Encoding utf8 -Append $log
      [ShotE]::Shot($p.MainWindowHandle, (Join-Path $OutDir 'elevated-power.png'))
      "captured elevated Power & Freq window" | Out-File -Encoding utf8 -Append $log
      Stop-Process -Id $p.Id -Force
    } else { "elevated gotm exited early: $($p.ExitCode)" | Out-File -Encoding utf8 -Append $log }
  } catch {
    "capture failed: $($_.Exception.Message)" | Out-File -Encoding utf8 -Append $log
    if ($p -and -not $p.HasExited) { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue }
  } finally {
    if ($null -eq $had) { Remove-ItemProperty $key -Name RyzenSDK -ErrorAction SilentlyContinue }
    else { Set-ItemProperty $key -Name RyzenSDK -Value $had -Type DWord }
  }
}
'@
$innerPath = Join-Path $OutDir 'elevated-inner.ps1'
Set-Content -Path $innerPath -Value $inner -Encoding UTF8
$appExe = if ($NoApp) { '' } else { Join-Path $root 'dist\gotm.exe' }
$args = @('-NoProfile','-ExecutionPolicy','Bypass','-File',$innerPath,'-TestExe',$testExe,'-OutDir',$OutDir,'-AppExe',$appExe)
Write-Host "Requesting administrator rights (UAC prompt)..."
$proc = Start-Process -FilePath 'powershell.exe' -ArgumentList $args -Verb RunAs -PassThru -Wait
Write-Host "elevated run finished (exit $($proc.ExitCode)); log: $OutDir\elevated-tests.txt"
if (Test-Path (Join-Path $OutDir 'elevated-tests.txt')) { Get-Content (Join-Path $OutDir 'elevated-tests.txt') }
