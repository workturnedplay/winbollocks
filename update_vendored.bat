@echo off
setlocal enabledelayedexpansion

echo [1/4] Updating all dependencies...
go get -u ./...
if %errorlevel% neq 0 (set "stage=Update" & goto :failed)

echo [2/4] Cleaning up go.mod...
go mod tidy
if %errorlevel% neq 0 (set "stage=Tidy" & goto :failed)

if exist vendor (
    echo [3/4] Cleaning old vendor files...
    rd /s /q vendor
    if exist vendor (
        set "stage=Vendor Cleanup (Directory might be locked by another process)"
        goto :failed
    )
) else (
    echo [3/4] No vendor folder found, skipping cleanup...
)

echo [4/4] Creating fresh vendor folder...
go mod vendor
if %errorlevel% neq 0 (set "stage=Vendor" & goto :failed)

echo.
echo ========================================
echo SUCCESS: All dependencies updated and vendored.
echo ========================================
pause
exit /b 0

:failed
echo.
echo ########################################
echo ERROR: %stage% failed with exit code %errorlevel%.
echo ########################################
pause
exit /b %errorlevel%
