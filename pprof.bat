echo top -cum | go tool pprof heap_final.prof
( echo sample_index=alloc_space
echo top -cum
) | go tool pprof heap_final.prof
pause