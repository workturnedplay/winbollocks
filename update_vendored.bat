@echo off
setlocal enabledelayedexpansion
:: Yes. Since you have setlocal enabledelayedexpansion at the top, using !VAR! is the "gold standard" for making your script refactor-proof.
::In Batch, %VAR% is expanded when a command (or a block of commands inside parentheses) is read, while !VAR! is expanded when the command is executed.

:: 1. Get Installed Go Version (e.g., 1.26.0)
:: 1. Get the REAL Go version on your SSD (bypassing go.mod requirements)
set "OLD_GTC=!GOTOOLCHAIN!"
set "GOTOOLCHAIN=local"
set "INSTALLED_VER="
set "PROJECT_VER="

for /f "tokens=3" %%v in ('go version') do (
    set "INSTALLED_VER=%%v"
    set "INSTALLED_VER=!INSTALLED_VER:go=!"
)
set "GOTOOLCHAIN=!OLD_GTC!"

:: 2. Safety check: Did 'go version' actually work?
if "!INSTALLED_VER!"=="" (
    set "stage=Local Go Check (Is Go installed and in your PATH?)"
    goto :failed
)

:: Validate Installed Version
set "CHECK_TARGET=!INSTALLED_VER!"
call :ValidateVersion
if !errorlevel! neq 0 (set "stage=Installed Version Validation" & goto :failed)

:: 3. Get Current Project Go Version from go.mod
set "PROJECT_VER="
if exist go.mod (
    for /f "tokens=2" %%v in ('findstr /b "go " go.mod') do (
				::By default, FOR /F treats spaces and tabs as delimiters and collapses them. This means the variable %%v is usually "pre-trimmed" of horizontal whitespace before it even touches your set command.
        set "PROJECT_VER=%%v"
    )
)

if "!PROJECT_VER!"=="" (
    set "stage=Read go.mod (File missing or 'go' line not found)"
    goto :failed
)

:: Validate Project Version
set "CHECK_TARGET=!PROJECT_VER!"
call :ValidateVersion
if !errorlevel! neq 0 (set "stage=Project Version Validation" & goto :failed)

echo Checking 'go version' alignment...
echo Installed Go        : !INSTALLED_VER!
echo Project's minimum Go: !PROJECT_VER!

:: 3. PROPER COMPARISON using PowerShell's [version] type
:: This handles 1.26.0 vs 1.5.0 correctly because it compares them as numbers, not strings.
::powershell -command "if ([version]'!INSTALLED_VER!' -gt [version]'!PROJECT_VER!') { exit 0 } else { exit 1 }" >nul 2>&1
powershell -command "$v1 = '!INSTALLED_VER!'.Split('-')[0]; $v2 = '!PROJECT_VER!'.Split('-')[0]; if ([version]$v1 -gt [version]$v2) { exit 0 } else { exit 1 }" >nul 2>&1
::powershell -command "$v1 = '!PROJECT_VER!'.Split('-')[0]; $v2 = '!INSTALLED_VER!'.Split('-')[0]; if ([version]$v1 -gt [version]$v2) { exit 0 } else { exit 1 }" >nul 2>&1
set "COMPARE_RESULT=!errorlevel!"

if "!COMPARE_RESULT!"=="0" (
    echo [0/4] Bumping go.mod to !INSTALLED_VER!...
    go mod edit -go=!INSTALLED_VER!
    if !errorlevel! neq 0 (set "stage=Go Version Bump" & goto :failed)
) else (
    :: Check if Project > Installed (The "Future Version" problem)
    powershell -command "if ([version]'!PROJECT_VER!' -gt [version]'!INSTALLED_VER!') { exit 0 } else { exit 1 }" >nul 2>&1
    if !errorlevel! equ 0 (
        echo [!] WARNING: go.mod wants !PROJECT_VER!, but you only have !INSTALLED_VER!.
        echo [!] Go will attempt to download the toolchain now...
				go version
				if !errorlevel! neq 0 (set "stage=Go version self-updating from the internet" & goto :failed)
    ) else (
        echo [0/4] Versions match. Skipping bump.
    )
)

:: 5. Update and Sync (Standard workflow)
echo [1/4] Updating all dependencies... needs internet access to check if new versions are available.
go get -u ./...
if !errorlevel! neq 0 (set "stage=Dependencies Update (needs internet access)" & goto :failed)

echo [2/4] Cleaning up go.mod...
go mod tidy
if !errorlevel! neq 0 (set "stage=Tidy" & goto :failed)

echo [3/4] Not deleting vendor folder.

echo [4/4] Updating vendor folder...
go mod vendor
if !errorlevel! neq 0 (set "stage=Creating and populating 'vendor' dir" & goto :failed)

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


::shouldn't be reached:
exit /b 111
:ValidateVersion
:: 1. Extract only the part before a hyphen (e.g., 1.26.0-rc1 -> 1.26.0)
for /f "tokens=1 delims=-" %%a in ("!CHECK_TARGET!") do set "V_RAW=%%a"

:: 2. Nuclear whitespace/garbage trim
for /f "delims=" %%a in ("!V_RAW!") do set "V_RAW=%%a"
set "V_RAW=!V_RAW: =!"

:: 3. PowerShell Regex Validation
:: This checks if the REMAINING string is ONLY digits and dots.
powershell -command "if ('!V_RAW!' -match '^[0-9.]+$') { exit 0 } else { exit 1 }" >nul 2>&1

if !errorlevel! neq 0 (
    ::echo ERROR: Version "!CHECK_TARGET!" (Cleaned: "!V_RAW!") contains illegal characters.
		echo ERROR: Version !CHECK_TARGET! is invalid.
    echo Cleaned string was: !V_RAW!
    exit /b 1
)
exit /b 0