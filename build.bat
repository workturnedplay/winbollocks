@echo off

echo Running go vet...
:: ./... means “Walk the directory tree from here, find every Go package, and apply vet to each.”
:: 'go vet' does:
:: Full static analysis of the package
:: Including unreachable code
:: Including dead branches
:: Including code not exercised by tests
::go vet -mod=vendor ./...
go vet -mod=vendor -unsafeptr=false
if errorlevel 1 goto :fail

go build -mod=vendor -ldflags="-H=windowsgui" .
if errorlevel 1 goto :fail

::When you build with: -ldflags "-H=windowsgui"
::your binary is linked as a GUI subsystem executable (/SUBSYSTEM:WINDOWS), not a console subsystem one.
::Consequences:
::• No console is allocated
::• stdout, stderr, and stdin do not exist
::• panic, fmt.Println, log.Fatal, etc. have nowhere to write
::• Windows shows nothing unless you explicitly create UI or log to disk

::Without -H=windowsgui (default):
::• Subsystem: Console
::• A console window is attached or created
::• fmt.Println, panic, logging all work
::• On launch, a black console window appears
::• Required for CLI tools or debugging
::
::With -H=windowsgui:
::
::• Subsystem: Windows GUI
::• No console window
::• Silent unless you implement logging or UI
::• Correct for tray apps, background tools, shell helpers
::• Required if you want “invisible” behavior
::
::Nothing else changes.
::No runtime behavior differences.
::No threading differences.
::No input differences.
::
::This flag affects only how Windows initializes the process.



echo Build succeeded.
pause
goto :eof

:fail
echo.
echo *** BUILD FAILED ***
pause
exit /b 1