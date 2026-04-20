@echo off

:: (nope:)disallow Ctrl+break, no effect, it still prompts: Terminate batch job (Y/N)?
::break off
::so, break off:
::Does not disable Ctrl+Break
::Does not prevent interruption
::Does not affect Ctrl+C at all
::Only controls whether Ctrl+Break sets the internal BREAK flag
::That flag is checked by certain batch commands (FOR, COPY, etc.) to decide whether to abort early.


:: ctrl+c is trapped by our .exe by putting the terminal in raw mode, thus this .bat won't sense it and ask to terminate batch job.

setlocal EnableExtensions EnableDelayedExpansion


@rem set GOMAXPROCS=12

@rem set CGO_ENABLED=1
@rem go run -race main.go

@rem pause
echo Current working directory is on next line:
cd
echo Script is running from "%~dp0"
rem cd /d is a built-in that parses the path differently, it accepts the trailing ^ literally and changes the working directory.
rem No, lol, it's because of this: "When you do just echo "%~dp0", CMD treats %~dp0 as a standalone token inside quotes, and it preserves the trailing ^ because it’s not immediately followed by another character. So you see the caret in your output. But when you do concatenation... caret is interpreted as an escape → lost."
cd /d "%~dp0"
:: What %~dp0 actually is
:: %0 → the path used to launch the script
:: ~d → drive letter
:: ~p → path (ending with a backslash)
::
:: cd /d changes the driver letter too
:: "Use the /D switch to change current drive in addition to changing current directory for a drive."
echo Current^(changed^) working directory is on next line:
cd

rem set "READCFG_PRIME=1" not needed anymore
rem call readcfg.bat
rem even tho we are in %~dp0 already, still doing this to be sure, doesn't work due to "^"(in dir name) getting eaten.
rem call "%~dp0\readcfg.bat"
for %%I in (.) do (
    if /i "%%~fI\" NEQ "%~dp0" (
        echo Current dir^(1^) does NOT match script dir^(2^) ie. cd /d must've failed earlier, thus we don't want to accidentally call a .bat from the wrong dir.
        for %%I in (.) do echo 1: "%%~fI"
        echo 2: "%~dp0"
    )
)
call ".\readcfg.bat" wtw
set "ec=%ERRORLEVEL%"
if "!ec!" NEQ "0" (
  echo Couldn't find readcfg.bat in "%~dp0"
  pause
  exit /b 1
)

if "!log_file!" NEQ "" (
  if exist "!log_file!" (
    echo Cleared log file: "!log_file!"
    type nul > "!log_file!"
  )
)

@rem %~dp0 already has the end \ but adding another one for visibility:
:run
:: this variant eats the "^" in the dir name:
rem set "cmd=%~dp0!exe_name!"
::no effect because it's missing:
rem set "cmd=!cmd:^=^^!"
:: this variant works:
::pushd "%~dp0"
:: escape any ^ characters
rem echo Checking for the existence of "!exe_name!" in dir "%~dp0" ...
:: Check if the file actually exists first
if not exist "!exe_name!" (
    echo Error: Could not find "!exe_name!" in current dir^(seen above^)
    pause
    exit /b
)

set GOTRACEBACK=all
set WINCOE_SMASHY_TEST=1
set WINCOE_SMASHY_RUNGC=1
rem set GODEBUG=gctrace=1,gc=1,allocfreetrace=1
set "GORACE=halt_on_error=1:log_path=race.log"
rem won't see it: go env GORACE
echo GORACE is '%GORACE%'

echo Running command^(in current dir^): "!exe_name!"
"!exe_name!"
set "ec=%ERRORLEVEL%"

if "!ec!"=="130" (
    echo "!exe_name!" exited with code 130 ^(sigint^) - which to this bat file means we should be restarting it... ^(use alt+x to not do this next time^)
    goto run
)

if "!ec!"=="0" (
    echo "!exe_name!" finished successfully.
) else (
    echo "!exe_name!" exited with error code "!ec!"
    if "!log_file!" NEQ "" (
      if exist "!log_file!" (
        echo ---- debug log file "!log_file!" echoed below ----
        type "!log_file!"
        echo ---- debug log file "!log_file!" echoed above ----
      )
    )
)
pause
