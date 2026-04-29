@echo off
cd /d "%~dp0" 2>nul || rem Stay in current directory if already correct

set "bang=^!"
setlocal enabledelayedexpansion
rem  Yes. Since you have setlocal enabledelayedexpansion at the top, using !VAR! is the "gold standard" for making your script refactor-proof.
rem In Batch, %VAR% is expanded when a command (or a block of commands inside parentheses) is read, while !VAR! is expanded when the command is executed.

rem  1. Get Installed Go Version (e.g., 1.26.0)
rem  1. Get the REAL Go version on your SSD (bypassing go.mod requirements)
set "OLD_GTC=!GOTOOLCHAIN!"
set "GOTOOLCHAIN=local"

rem this GOPRIVATE is needed because of Go 1.27 requirement in wincoe which isn't yet released thus gosumdb will 404 it.
rem FIXME: remove it after Go 1.27 is released so it can get it via gosumdb/proxy
set "GOPRIVATE=github.com/workturnedplay/*"

set "INSTALLED_VER="
set "PROJECT_VER="
rem  0. Capture Workspace State
rem  Run this BEFORE you 'set GOWORK=off' if you want to know the original state
set "WS_PATH="
for /f "tokens=*" %%w in ('go env GOWORK') do set "WS_PATH=%%w"

rem  If WS_PATH is "off" or empty, we aren't in a workspace.
rem  Otherwise, WS_PATH contains the full path to your go.work file.
if NOT "!WS_PATH!"=="off" if NOT "!WS_PATH!"=="" (
    set "HAS_WORKSPACE=1"
    rem Extract the directory from the full file path
    echo Detected Workspace: !WS_PATH!
) else (
    set "HAS_WORKSPACE=0"
)

rem  Now you can safely disable GOWORK for the rest of the script
set "GOWORK=off"
rem echo on
set "IS_DEVEL=0"
for /f "tokens=3" %%v in ('go version') do (
    set "INSTALLED_VER=%%v"
    set "INSTALLED_VER=!INSTALLED_VER:go=!"
    
    rem Check if the original version string contains "devel"
    echo !INSTALLED_VER! | findstr /i "devel" >nul && set "IS_DEVEL=1"
    
    rem 1b. Create CLEAN_GO_VERSION for use in go.mod / go.work (strip devel + commit)
    rem For devel builds like 1.27-devel_081aa64e61, we want "1.27" (or "1.27.0" if you prefer)
    set "CLEAN_INSTALLED_VER=!INSTALLED_VER!"
    rem First, remove everything after the first '-' (devel suffix and hash)
    for /f "tokens=1 delims=-" %%a in ("!CLEAN_INSTALLED_VER!") do set "CLEAN_INSTALLED_VER=%%a"
)
set INSTALLED_VER=!CLEAN_INSTALLED_VER!
set "GOTOOLCHAIN=!OLD_GTC!"

rem  2. Safety check: Did 'go version' actually work?
if "!INSTALLED_VER!"=="" (
    set "stage=Local Go Check (Is Go installed and in your PATH?)"
    goto :failed
)

rem Validate Installed Version
set "CHECK_TARGET=!INSTALLED_VER!"
call :ValidateVersion
if !errorlevel! neq 0 (set "stage=Installed Version Validation" & goto :failed)

rem 3. Get Current Project Go Version from go.mod
set "PROJECT_VER="
if exist go.mod (
    for /f "tokens=2" %%v in ('findstr /b "go " go.mod') do (
        rem By default, FOR /F treats spaces and tabs as delimiters and collapses them. This means the variable %%v is usually "pre-trimmed" of horizontal whitespace before it even touches your set command.
        set "PROJECT_VER=%%v"
    )
)

if "!PROJECT_VER!"=="" (
    set "stage=Read go.mod (File missing or 'go' line not found)"
    goto :failed
)

rem Validate Project Version
set "CHECK_TARGET=!PROJECT_VER!"
call :ValidateVersion
if !errorlevel! neq 0 (set "stage=Project Version Validation" & goto :failed)

echo Checking 'go version' alignment...
echo Installed Go        : !INSTALLED_VER!
echo Project's minimum Go: !PROJECT_VER!

if "!IS_DEVEL!"=="1" (
        echo [0/4] Devel version detected "!INSTALLED_VER!" ergo skipping go.mod bump.
  ) else (
rem 3. PROPER COMPARISON using PowerShell's [version] type
rem  This handles 1.26.0 vs 1.5.0 correctly because it compares them as numbers, not strings.
rem powershell -command "if ([version]'!INSTALLED_VER!' -gt [version]'!PROJECT_VER!') { exit 0 } else { exit 1 }" >nul 2>&1
powershell -NoProfile -command "$v1 = '!INSTALLED_VER!'.Split('-')[0]; $v2 = '!PROJECT_VER!'.Split('-')[0]; if ([version]$v1 -gt [version]$v2) { exit 0 } else { exit 1 }" >nul 2>&1
rem powershell -command "$v1 = '!PROJECT_VER!'.Split('-')[0]; $v2 = '!INSTALLED_VER!'.Split('-')[0]; if ([version]$v1 -gt [version]$v2) { exit 0 } else { exit 1 }" >nul 2>&1
set "COMPARE_RESULT=!errorlevel!"

if "!COMPARE_RESULT!"=="0" (
    echo [0/4] Bumping go.mod to !INSTALLED_VER!...
    go mod edit -go=!INSTALLED_VER!
    if !errorlevel! neq 0 (set "stage=Go Version Bump" & goto :failed)
) else (
    rem Check if Project > Installed (The "Future Version" problem)
    powershell -NoProfile -command "if ([version]'!PROJECT_VER!' -gt [version]'!INSTALLED_VER!') { exit 0 } else { exit 1 }" >nul 2>&1
    if !errorlevel! equ 0 (
        echo [!] WARNING: go.mod wants !PROJECT_VER!, but you only have !INSTALLED_VER!.
        echo [!] Go will attempt to download the toolchain now...
        go version
        if !errorlevel! neq 0 (set "stage=Go version self-updating from the internet" & goto :failed)
    ) else (
        echo [0/4] Versions match. Skipping bump.
    )
)


rem 4.5 Robustly update go.work in the parent directory
rem 4.5 Update go.work only if we found one
if "!HAS_WORKSPACE!"=="1" (
  if exist "!WS_PATH!" (
      set "WORK_VER="
      rem Find the line starting with "go " in the parent go.work
      for /f "tokens=2" %%v in ('findstr /b "go " "!WS_PATH!"') do set "WORK_VER=%%v"

      if "!WORK_VER!"=="" (
          echo [!] WARNING: go.work found but no 'go' version line detected. Skipping.
      ) else (
          rem Validate the version string found in go.work
          set "CHECK_TARGET=!WORK_VER!"
          call :ValidateVersion
          if !errorlevel! neq 0 (set "stage=go.work Version Validation" & goto :failed)

          rem Compare: Is Installed > go.work?
          powershell -NoProfile -command "$v1 = '!INSTALLED_VER!'.Split('-')[0]; $v2 = '!WORK_VER!'.Split('-')[0]; if ([version]$v1 -gt [version]$v2) { exit 0 } else { exit 1 }" >nul 2>&1
          
          if !errorlevel! equ 0 (
              echo [0.5/4] Bumping parent go.work from !WORK_VER! to !INSTALLED_VER! in file !WS_PATH!
              go work edit -go=!INSTALLED_VER! "!WS_PATH!"
              if !errorlevel! neq 0 (set "stage=go.work Version Bump execution" & goto :failed)
          ) else (
              echo [0.5/4] Workspace version !WORK_VER! is already up to date.
          )
      )
  ) else (
    echo "%bang%%bang%%bang% Has GO workspace but the path doesn't exist: !WS_PATH!"
  )
)
)
rem above is end of 'if' it isn't Go devel version

rem  5. Update and Sync (Standard workflow)
echo [1/4] Updating all dependencies... needs internet access to check if new versions are available.
go get -u ./...
if !errorlevel! neq 0 (set "stage=Dependencies Update (needs internet access, go.exe and C:\Program Files\Git\mingw64\libexec\git-core\git-remote-https.exe due to GOPRIVATE must can do TCP 443)" & goto :failed)

echo [2/4] Cleaning up go.mod...
go mod tidy
if !errorlevel! neq 0 (set "stage=Tidy" & goto :failed)

echo [3/4] Not deleting vendor folder.

echo [4/4] Updating vendor folder...
go mod vendor
if !errorlevel! neq 0 (set "stage=Creating and populating 'vendor' dir" & goto :failed)

if "!HAS_WORKSPACE!"=="1" (
  echo [5/4] Detected Workspace. Syncing Workspace Vendor...
  rem We must temporarily re-enable GOWORK so the command knows which workspace to vendor
  set "GOWORK=!WS_PATH!"
  go work vendor
  if !errorlevel! neq 0 (set "stage=Workspace Vendoring" & goto :failed)
)

echo.
echo ========================================
echo SUCCESS: All dependencies updated and vendored.
echo ========================================
pause
exit /b 0

:failed
echo.
echo ########################################
echo ERROR: !stage! failed with exit code !errorlevel!.
echo ########################################
pause
exit /b !errorlevel!


rem shouldn't be reached:
exit /b 111
:ValidateVersion
rem  1. Extract only the part before a hyphen (e.g., 1.26.0-rc1 -> 1.26.0)
for /f "tokens=1 delims=-" %%a in ("!CHECK_TARGET!") do set "V_RAW=%%a"

rem  2. Nuclear whitespace/garbage trim
for /f "delims=" %%a in ("!V_RAW!") do set "V_RAW=%%a"
set "V_RAW=!V_RAW: =!"

rem  3. PowerShell Regex Validation
rem  This checks if the REMAINING string is ONLY digits and dots.
powershell -NoProfile -command "if ('!V_RAW!' -match '^[0-9.]+$') { exit 0 } else { exit 1 }" >nul 2>&1

if !errorlevel! neq 0 (
    rem echo ERROR: Version "!CHECK_TARGET!" (Cleaned: "!V_RAW!") contains illegal characters.
    echo ERROR: Version !CHECK_TARGET! is invalid.
    echo Cleaned string was: !V_RAW!
    exit /b 1
)
exit /b 0
