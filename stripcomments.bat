@echo off
go version
del tmpnocomments.go 2>nul
pushd ..\dnsbollocks\cmd\stripcomments-in-Go
go fmt
go build
go run -- main.go  -o ../../../winbollocks/tmpnocomments.go "../../../winbollocks/main.go"
echo exit code: %ERRORLEVEL%
popd
pause