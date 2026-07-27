@echo off
rem 1. Prevent the current working directory from taking precedence over PATH, doesn't work with eg. "start go.exe"
set "NoDefaultCurrentDirectoryInExePath=1"
::if running as admin must get back to current dir:
cd /d %~dp0
setlocal enabledelayedexpansion

echo Running memory benchmarks for WinCoe...
echo =======================================

:: -run=^$   : Skips normal unit tests, matches nothing
:: -bench=.  : Runs all functions starting with "Benchmark"
:: -benchmem : Includes memory allocation statistics (CRUCIAL)
:: -count=1  : Runs the suite once
go test -run="SKIPALLTESTSIWANTTHEBENCHMARKSONLY" -bench=. -benchtime=1s -benchmem -count=2 ./...
rem go test -bench=. -benchmem ./...

if errorlevel 1 goto :fail

echo.
echo All benchmarks completed successfully.
pause
goto :eof

:fail
echo.
echo *** Benchmarks FAILED ***
pause
exit /b 1