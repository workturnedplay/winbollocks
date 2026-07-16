@echo off
setlocal enabledelayedexpansion

rem I put custom Go in PATH
set "goexe=go.exe"
rem set "goexe=D:\custom-go\go\bin\go.exe"
if NOT "%1" == "silent" (
  echo Using GO exe as: %goexe%
  "%goexe%" version
  set | findstr GO
)
rem shouldn't see anything other than GOPATH being set, if GOROOT is set then we've a problem for gcc might use it?! unsure


REM REM 0. Capture Workspace State
REM REM Run this BEFORE you 'set GOWORK=off' if you want to know the original state
REM set "WS_PATH="
REM for /f "tokens=*" %%w in ('go env GOWORK') do set "WS_PATH=%%w"

REM rem If WS_PATH is "off" or empty, we aren't in a workspace.
REM rem Otherwise, WS_PATH contains the full path to your go.work file.
REM if NOT "!WS_PATH!"=="off" if NOT "!WS_PATH!"=="" (
    REM set "HAS_WORKSPACE=1"
    REM if NOT "%1" == "silent" (
      REM rem Extract the directory from the full file path
      REM echo Detected Workspace: !WS_PATH!
    REM )
REM ) else (
    REM set "HAS_WORKSPACE=0"
REM )

REM if "!HAS_WORKSPACE!"=="1" (
  REM set "MOD_FLAG="
  REM if NOT "%1" == "silent" (
    REM echo Running unvendored due to workspace
  REM )
REM ) else (
  REM rem Use vendor ONLY if we are NOT in a workspace
  REM set "MOD_FLAG=-mod=vendor"
  REM if NOT "%1" == "silent" (
    REM echo Running vendored due to lack of workspace
    REM rem This is the long-form flag the linter actually understands
  REM )
  REM set "LINT_MOD_FLAG=--modules-download-mode=vendor"
REM )

if NOT "%1" == "silent" (
  echo Running vendored
)
set "MOD_FLAG=-mod=vendor"
set "LINT_MOD_FLAG=--modules-download-mode=vendor"

echo Running go vet...
:: ./... means “Walk the directory tree from here, find every Go package, and apply vet to each.”
:: 'go vet' does:
:: Full static analysis of the package
:: Including unreachable code
:: Including dead branches
:: Including code not exercised by tests
::go vet -mod=vendor ./...
"%goexe%" vet !MOD_FLAG! -unsafeptr=false
if errorlevel 1 goto :fail

echo Running go vet for shadowing ...
set "shade=%USERPROFILE%\go\bin\shadow.exe"
:: Check if shadow.exe exists, if not, install it
if not exist "%shade%" (
    echo [!] shadow.exe not found. Installing via go install...
    "%goexe%" install golang.org/x/tools/go/analysis/passes/shadow/cmd/shadow@latest
    
    :: Double check if installation actually succeeded
    if not exist "%shade%" (
        echo [ERROR] Failed to install shadow analyzer. Check your internet/DNS.
        exit /b 1
    )
)
"%goexe%"  vet -vettool="%shade%"
if errorlevel 1 goto :fail

echo Running go vet on everything...
"%goexe%" vet !MOD_FLAG! -unsafeptr=false ./...
if errorlevel 1 goto :fail

set "lintexe=%USERPROFILE%\go\bin\golangci-lint.exe"
rem :: Check if golangci-lint.exe exists, if not, install it
if not exist "%lintexe%" (
    echo [!] golangci-lint.exe not found. Installing via go install...
    "%goexe%" install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
    
    :: Double check if installation actually succeeded
    if not exist "%lintexe%" (
        echo [ERROR] Failed to install golangci-lint. Check your internet/DNS.
        exit /b 1
    )
)
echo Running %lintexe% run !LINT_MOD_FLAG! ./...
"%lintexe%" run !LINT_MOD_FLAG! ./...
if errorlevel 1 goto :fail


echo Check succeeded.
pause
goto :eof

:fail
echo.
echo *** CHECK FAILED ***
pause
exit /b 1

