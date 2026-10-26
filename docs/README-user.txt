gotm — a native task manager for Windows
=======================================

Requirements
------------
Windows 10 or Windows 11, 64-bit (x64). Nothing else to install.

Installing
----------
Installer:  run gotm-<version>-setup.exe and approve the Windows prompt. gotm is
            installed for all users in "C:\Program Files\gotm", with a Start menu
            shortcut. Run it again later to upgrade in place.
            Silent install for IT deployment:   gotm-<version>-setup.exe /S
            Also add a desktop shortcut:        gotm-<version>-setup.exe /desktop
            Custom folder:                      gotm-<version>-setup.exe /D=C:\Protected\gotm
            Custom folders and their parents must have trusted ownership and
            protection against standard-user changes. Setup rejects unsafe or
            redirected paths, including during upgrades.

Portable:   unzip gotm-<version>-portable.zip anywhere and run gotm.exe.
            Settings are stored per user in %APPDATA%\gotm either way.

"Windows protected your PC"
---------------------------
gotm is not code-signed, so Microsoft Defender SmartScreen may warn the first
time it runs on a computer. Check that the file's SHA-256 matches the
SHA256SUMS.txt you were given, then choose "More info" > "Run anyway".
(For a downloaded zip you can also right-click it > Properties > Unblock
before extracting.)

Using it
--------
- The sidebar lists the pages. Ctrl+1 … Ctrl+0 jump between them, Ctrl+F
  searches the current page, F5 reloads it. Settings lists every shortcut.
- Flight Recorder starts stopped each time gotm launches. Select "Start
  recording" on its page, press Space on the chart, or click "REC off" in the
  status bar to begin. The disk-saving setting does not start recording.
- Ending tasks, controlling services and changing startup entries for other
  users needs administrator rights: click "Run as administrator" in the
  bottom-right corner and gotm restarts elevated.
- Power & Freq shows clocks, drive health, GPU temperatures/clocks/power
  (NVIDIA and AMD, read from the graphics driver you already have) and, where
  Windows exposes it, CPU package power.
  CPU die temperature needs a vendor sensor driver; gotm never installs one.
  Optional sources: AMD Ryzen Master Monitoring SDK (Settings > AMD Ryzen
  sensors, Ryzen CPUs only, run as administrator) or LibreHardwareMonitor with
  its web server on 127.0.0.1:8085.
- Accessibility: every page works with the keyboard alone (Tab, arrows,
  Enter), screen readers such as Narrator read the cards, charts captions and
  settings, and a Windows high-contrast theme is used as soon as it is on.

Use gotm as Windows Task Manager
-------------------------------
Install gotm first, open its installed copy and choose "Run as administrator".
In Settings > Windows Task Manager, select "Replace Task Manager". This applies
to every user on the PC. Task Manager launches, including Ctrl+Shift+Esc, open
gotm's Processes page. Replacement stays off until you enable it explicitly.

Select "Restore Windows Task Manager" there to turn it off. Uninstall restores
gotm's own routing before deleting its files. Other programs' replacements and
launch filters are preserved; restore those through their own program first.
Portable copies cannot enable replacement.

If gotm cannot open, run this in an administrator PowerShell window (use your
actual folder if you installed to a protected custom location):
  & 'C:\Program Files\gotm\uninstall.exe' /taskmanager=off
This also works if gotm.exe is missing.

Privacy
-------
gotm makes no internet connections and collects nothing. The only network
access is optional and local: reading LibreHardwareMonitor on 127.0.0.1.
The Flight Recorder captures only after you start it manually for the current
app session. It keeps its history in memory unless you turn on "Keep recording
on disk" (stored in %APPDATA%\gotm). Saved history can be viewed before starting
a new recording.

Removing
--------
Settings > Apps > gotm > Uninstall (or run "C:\Program Files\gotm\uninstall.exe").
Uninstall asks whether to delete your settings too; it never deletes files
it did not install. Portable copy: just delete the folder.

Third-party software
--------------------
THIRD-PARTY-NOTICES.txt lists the open-source components gotm is built from and
their licenses.
