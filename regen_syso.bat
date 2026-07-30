@echo off
rem 1. Prevent the current working directory from taking precedence over PATH, doesn't work with eg. "start go.exe"
set "NoDefaultCurrentDirectoryInExePath=1"

setlocal enabledelayedexpansion

::if running as admin must get back to current dir:
cd /d %~dp0

go run gen_ico_resource.go
echo "exit code %ERRORLEVEL%"
rem echo "probably (re)made rsrc_windows_amd64.syso"
pause