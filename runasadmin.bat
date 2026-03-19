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
echo Running command^(in current dir^): "!exe_name!"
echo Requesting elevation for command "!exe_name!"... (spawns in new cmd.exe window, doesn't wait for it here)
rem !exe_name!
::powershell -Command "Start-Process cmd -ArgumentList '/k \"\"!exe_name!\"\"' -Verb RunAs"
powershell -NoProfile -Command "$wd = (Get-Location).Path; $exe = $env:EXE_NAME; Start-Process -FilePath (Join-Path -Path $wd -ChildPath $exe) -WorkingDirectory $wd -Verb RunAs"
set "ec=%ERRORLEVEL%"

if "!ec!"=="0" (
    :: ^( is ( but escaped, lame i kno.
    echo Started it successfully in a new cmd.exe^(as admin^) window^(which lingers only if it's the devbuild and thus has a console^).
) else (
    echo couldn't start it, exited with error code "!ec!"
)
pause
