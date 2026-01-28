echo top -cum | go tool pprof cpu.prof
( echo sample_index=alloc_space
echo top -cum
) | go tool pprof cpu.prof
pause