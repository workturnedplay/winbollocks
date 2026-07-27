@echo off
rem 1. Prevent the current working directory from taking precedence over PATH, doesn't work with eg. "start go.exe"
set "NoDefaultCurrentDirectoryInExePath=1"
::if running as admin must get back to current dir:
cd /d %~dp0
setlocal enabledelayedexpansion


echo "this executes the code in gen_*.go file! Ctrl+C to stop or any other normal key to continue"
pause
go generate ./...
pause