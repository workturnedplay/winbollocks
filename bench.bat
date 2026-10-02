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
echo Current DIR: "!SCRIPT_DIR!"

go test -run TestKeyDownAllocs -v
go test -bench BenchmarkKeyDown -benchmem
go test -bench BenchmarkBoundProcKeyDown -benchmem

go test -bench BenchmarkBoundProcGetAsyncStateArity1 -benchmem
go test -bench BenchmarkDirectSyscallNKeyDown -benchmem
go test -bench BenchmarkWinCall1KeyDown -benchmem
pause