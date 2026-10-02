@echo off
rem 1. Prevent the current working directory from taking precedence over PATH, doesn't work with eg. "start go.exe"
set "NoDefaultCurrentDirectoryInExePath=1"

::if running as admin must get back to current dir:
:: 1. Change directory and store path BEFORE enabling delayed expansion.
::    While delayed expansion is disabled, '!' is treated as a literal character.
setlocal disabledelayedexpansion
:: ^ needed if called by a 'call me.bat' command where it's already enabled in parent!
cd /d "%~dp0"
set "SCRIPT_DIR=%~dp0"
set "scriptf0=%~f0"
rem endlocal
:: 2. Enable delayed expansion for script logic
rem setlocal enabledelayedexpansion

:: 3. Test: Use !SCRIPT_DIR! (safe), do NOT use %SCRIPT_DIR% or %~dp0 (unsafe)
rem echo Current DIR: "!SCRIPT_DIR!"

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
echo Script is running from "!SCRIPT_DIR!"
rem cd /d is a built-in that parses the path differently, it accepts the trailing ^ literally and changes the working directory.
rem No, lol, it's because of this: "When you do just echo "%~dp0", CMD treats %~dp0 as a standalone token inside quotes, and it preserves the trailing ^ because it’s not immediately followed by another character. So you see the caret in your output. But when you do concatenation... caret is interpreted as an escape → lost."
rem cd /d "%~dp0" XXX: we're already in here!
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
rem call .\readcfg.bat
rem even tho we are in %~dp0 already, still doing this to be sure, doesn't work due to "^"(in dir name) getting eaten.
rem call "%~dp0\readcfg.bat"
:: Compare !CD!\ against !SCRIPT_DIR! (note the trailing backslash addition)
if /i "!CD!\" NEQ "!SCRIPT_DIR!" (
    echo Current dir^(1^) does NOT match script dir^(2^) ie. cd /d must've failed earlier, thus we don't want to accidentally call a .bat from the wrong dir.
    echo 1: "!CD!"
    echo 2: "!SCRIPT_DIR!"
)
call ".\readcfg.bat" wtw
set "ec=%ERRORLEVEL%"
if "!ec!" NEQ "0" (
  echo Couldn't find readcfg.bat in current dir which is "!SCRIPT_DIR!" or it failed with exitcode !ec!
  pause
  exit /b 1
)

if "!winbollocks_log_file!" NEQ "" (
  if exist "!winbollocks_log_file!" (
    echo Cleared log file: "!winbollocks_log_file!"
    type nul > "!winbollocks_log_file!"
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
rem GORACE being unset won't skip the +1 sec delay on shutdown due to being compiled by 'go build -race'
rem set GORACE=
rem won't see it: go env GORACE
echo GORACE is '%GORACE%'

echo Running command^(in current dir^): "!exe_name!"
rem to handle ctrl+c without asking me "Terminate batch job (Y/N)?", we use powershell to run the .exe as follows // didn't work, doh!
rem & means always execute, not if exit is 0
"!exe_name!" & set "ec=!ERRORLEVEL!" & call :ignoreCtrlC

rem 1. Capture the ISO timestamp into a variable, actually this takes 0.23sec to run
rem for /f "delims=" %%i in ('powershell -Command "Get-Date -Format 'yyyy-MM-ddTHH:mm:ss.fffK'"') do set "ts=%%i"
:: This is nearly instantaneous (0ms delay)
:: Use the first 8 chars of time (HH:mm:ss)
rem set "ts=%DATE:~10,4%-%DATE:~4,2%-%DATE:~7,2%T%TIME: =0%"
rem The command set "t=%TIME: =0%" is a string replacement trick in Batch. It looks for any spaces in the %TIME% variable and replaces them with a zero.
set "t=%TIME: =0%"
set "ts=%DATE%T%t%"
:: Grab the pieces (this depends on your locale, but usually works)
:: If your date is "Fri 04/24/2026", ~10,4 is Year, ~4,2 is Month, ~7,2 is Day
rem set "iso_date=!DATE:~10,4!-!DATE:~4,2!-!DATE:~7,2!"
rem set "iso_time=!TIME: =0!"
rem set "ts=!iso_date!T!iso_time!"

if "!ec!"=="130" (
    echo time=!ts! "!exe_name!" exited with code 130 ^(sigint^) ie. via ctrl+break or ctrl+c - which to this bat file means we should be restarting it... ^(use alt+x to not do this next time^)
    pause
    goto run
)

if "!ec!"=="0" (
    echo time=!ts! "!exe_name!" finished successfully.
) else (
    echo time=!ts! "!exe_name!" exited with error code "!ec!"
    if "!winbollocks_log_file!" NEQ "" (
      if exist "!winbollocks_log_file!" (
        echo ---- debug log file "!winbollocks_log_file!" echoed below ----
        type "!winbollocks_log_file!"
        echo ---- debug log file "!winbollocks_log_file!" echoed above ----
      )
    )
)
pause

rem Prevents falling into the subroutine during a normal sequential run
goto :eof

:ignoreCtrlC
exit /b

