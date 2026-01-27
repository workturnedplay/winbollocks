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
echo "Current working directory is:"
cd
echo "Script is running from %~dp0"
cd /d %~dp0
:: What %~dp0 actually is
:: %0 → the path used to launch the script
:: ~d → drive letter
:: ~p → path (ending with a backslash)
::
:: cd /d changes the driver letter too
:: "Use the /D switch to change current drive in addition to changing current directory for a drive."

echo "Current(changed) working directory is:"
cd

@rem %~dp0 already has the end \ but adding another one for visibility:
:run
set cmd="%~dp0\winbollocks.exe"
:: Check if the file actually exists first
if not exist "%cmd%" (
    echo Error: Could not find %cmd%
    pause
    exit /b
)
echo Running %cmd%
echo Requesting elevation for %cmd%... (spawns in new cmd.exe window, doesn't wait for it here)
rem %cmd%
::powershell -Command "Start-Process cmd -ArgumentList '/k \"\"%cmd%\"\"' -Verb RunAs"
powershell -Command "Start-Process -FilePath '%cmd%' -WorkingDirectory '%~dp0' -Verb RunAs"
:: Disable QuickEdit mode for this console session, didn't work! done it in Go code then!
::powershell -NoProfile -Command "$st=(Get-ItemProperty 'HKCU:\Console').QuickEdit; Set-ItemProperty 'HKCU:\Console' 'QuickEdit' 0; start-process '%cmd%' -WorkingDirectory '%~dp0' -verb runas; Set-ItemProperty 'HKCU:\Console' 'QuickEdit' $st; exit"
set "ec=%ERRORLEVEL%"

if "!ec!"=="0" (
		:: ^( is ( but escaped, lame i kno.
    echo started it successfully in a new cmd.exe^(as admin^) window^(which lingers only if it's the devbuild and thus has a console^).
) else (
    echo couldn't start it, exited with error code !ec!
)
pause
