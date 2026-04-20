@echo off
echo building with race detector...

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

call build.bat
rem double pause here, it's ok
pause