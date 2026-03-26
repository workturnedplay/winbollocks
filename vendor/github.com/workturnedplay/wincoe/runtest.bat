@echo off
setlocal enabledelayedexpansion

:: 0. Capture Workspace State
:: Run this BEFORE you 'set GOWORK=off' if you want to know the original state
set "WS_PATH="
for /f "tokens=*" %%w in ('go env GOWORK') do set "WS_PATH=%%w"

:: If WS_PATH is "off" or empty, we aren't in a workspace.
:: Otherwise, WS_PATH contains the full path to your go.work file.
if NOT "!WS_PATH!"=="off" if NOT "!WS_PATH!"=="" (
    set "HAS_WORKSPACE=1"
    :: Extract the directory from the full file path
    echo Detected Workspace: !WS_PATH!
) else (
    set "HAS_WORKSPACE=0"
)

::if exist "..\go.work" (
if "!HAS_WORKSPACE!"=="1" (
  set "MOD_FLAG="
  echo Running unvendored due to workspace
) else (
  :: Use vendor ONLY if we are NOT in a workspace
  set "MOD_FLAG=-mod=vendor"
  echo Running vendored due to lack of workspace
)

::if running as admin must get back to current dir:
cd /d %~dp0

::echo Cleaning Go cache
::go clean -cache -modcache
::if errorlevel 1 goto :fail

echo Running go vet...
:: ./... means “Walk the directory tree from here, find every Go package, and apply vet to each.”
:: 'go vet' does:
:: Full static analysis of the package
:: Including unreachable code
:: Including dead branches
:: Including code not exercised by tests
::go vet -mod=vendor ./...
::go vet -mod=vendor -unsafeptr=false
go vet !MOD_FLAG! -unsafeptr=false ./...
if errorlevel 1 goto :fail

echo Running go build
go build !MOD_FLAG! .
if errorlevel 1 goto :fail

echo Running go test
go test !MOD_FLAG! -v
if errorlevel 1 goto :fail

echo all succeeded.
pause
goto :eof

:fail
echo.
echo *** something FAILED ***
pause
exit /b 1