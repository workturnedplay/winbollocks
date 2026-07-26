go test -run TestKeyDownAllocs -v
go test -bench BenchmarkKeyDown -benchmem
go test -bench BenchmarkBoundProcKeyDown -benchmem
go test -bench BenchmarkDirectSyscallNKeyDown -benchmem
go test -bench BenchmarkWinCall1KeyDown -benchmem
pause