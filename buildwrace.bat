@echo off
rem 1. Prevent the current working directory from taking precedence over PATH, doesn't work with eg. "start go.exe"
set "NoDefaultCurrentDirectoryInExePath=1"

::if running as admin must get back to current dir:
:: 1. Change directory and store path BEFORE enabling delayed expansion.
::    While delayed expansion is disabled, '!' is treated as a literal character.
setlocal disabledelayedexpansion
:: ^ needed if called by a 'call me.bat' command where it's already enabled in parent!
cd /d "%~dp0"
set "SCRIPT_DIR=%~dp0"

:: 2. Enable delayed expansion for script logic
setlocal enabledelayedexpansion

:: 3. Test: Use !SCRIPT_DIR! (safe), do NOT use %SCRIPT_DIR% or %~dp0 (unsafe)
rem echo Current DIR: "!SCRIPT_DIR!"

echo building with race detector... WARNING: this adds +1 second delay on exit!

rem go env GOARCH
set CGO_ENABLED=1
rem set GOARCH=amd64
rem set GOOS=windows
rem go env CC
rem go env GOARCH
rem go env GORACE
gcc --version
rem gcc (MinGW-W64 x86_64-ucrt-posix-seh, built by Brecht Sanders, r7) 15.2.0

set "BUILD_WITH_RACE_DETECTOR=-race"

rem only when running the exe: set "GORACE=halt_on_error=1:log_path=race.log"

call .\build.bat
rem double pause here, it's ok
pause