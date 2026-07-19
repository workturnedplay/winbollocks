@echo off
rem 1. Prevent the current working directory from taking precedence over PATH, doesn't work with eg. "start go.exe"
set "NoDefaultCurrentDirectoryInExePath=1"

echo top -cum | go tool pprof cpu.prof
( echo sample_index=alloc_space
echo top -cum
) | go tool pprof cpu.prof
pause