@echo off

type nul > winbollocks_debug.log

::if running as admin must get back to current dir:
cd /d %~dp0
::echo running from: %CD%
set GODEBUG=allocfreetrace=1
.\winbollocks.exe
if ERRORLEVEL 1 (
  echo ---- debug log file echoed below ----
  type winbollocks_debug.log
  echo ---- debug log file echoed above ----
)
::echo ec:%ERRORLEVEL%
pause