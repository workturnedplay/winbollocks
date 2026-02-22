@echo off
setlocal enabledelayedexpansion

echo [1/3] Updating all dependencies...
go get -u ./...
if %errorlevel% neq 0 (set "stage=Update" & goto :failed)

echo [2/3] Cleaning up go.mod...
go mod tidy
if %errorlevel% neq 0 (set "stage=Tidy" & goto :failed)

echo [3/3] Syncing vendor folder...
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
