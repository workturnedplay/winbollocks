@echo off

::if running as admin must get back to current dir:
cd /d %~dp0

.\winbollocks.exe
if ERRORLEVEL 1 type winbollocks_debug.log
::echo ec:%ERRORLEVEL%
pause