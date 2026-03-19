@echo off
::you must have EnableDelayedExpansion in caller, else this will err then pause then exit!
:: you must pass any args to can run this, like: call readcfg.bat anything

:: DEBUG here means pause before exit
::set "READCFG_PRIME=DEBUG"

if /I "%~1"=="" (
    echo This file is not meant to be run directly or you forgot to pass any args to it. Will run in DEBUG mode, or Ctrl+C here.
    set "READCFG_PRIME=DEBUG"
    setlocal EnableDelayedExpansion
    pause
    rem exit /b 1
)

rem can't use setlocal at all, so we count on the caller to have it
::setlocal EnableDelayedExpansion
set "TMPSCRIPT_dx_E4ZXH9LAH07QF1RNOKDL=alpha"
set "TMPSCRIPT_dx_E4ZXH9LAH07QF1RNOKDL=beta"
if not "%TMPSCRIPT_dx_E4ZXH9LAH07QF1RNOKDL%"=="!TMPSCRIPT_dx_E4ZXH9LAH07QF1RNOKDL!" (
    echo ERROR: delayed expansion is not enabled but it's needed.
    pause
    exit /b 1
)

rem temp IDs were generated in git bash via: tr -dc 'A-Z0-9' </dev/urandom | head -c 20
:: whitelist of allowed keys, user-settable (here!)
set "TMPSCRIPT_WHITELIST_E4ZXH9LAH07QF1RNOKDL=exe_name log_file"

::TODO: use TMPSCRIPT_*_E4ZXH9LAH07QF1RNOKDL
set "TMPSCRIPT_cfgfname_E4ZXH9LAH07QF1RNOKDL=readcfg.env"
set "TMPSCRIPT_cfg_E4ZXH9LAH07QF1RNOKDL=%~dp0%TMPSCRIPT_cfgfname_E4ZXH9LAH07QF1RNOKDL%"

if not exist "%TMPSCRIPT_cfg_E4ZXH9LAH07QF1RNOKDL%" (
    echo Config file "%TMPSCRIPT_cfgfname_E4ZXH9LAH07QF1RNOKDL%" missing from "%TMPSCRIPT_cfg_E4ZXH9LAH07QF1RNOKDL%"
    pause
    exit /b 1
)

for /f "usebackq tokens=1,* delims==" %%A in ("%TMPSCRIPT_cfg_E4ZXH9LAH07QF1RNOKDL%") do (
    call :trim "%%A" TMP_key_N8RG305DWBRF52TCWV41
    call :trim "%%B" TMP_val_N8RG305DWBRF52TCWV41
    
    :: check key against whitelist
    set "TMPSCRIPT_found_E4ZXH9LAH07QF1RNOKDL=0"
    for %%W in (!TMPSCRIPT_WHITELIST_E4ZXH9LAH07QF1RNOKDL!) do (
        if /i "%%W"=="!TMP_key_N8RG305DWBRF52TCWV41!" set "TMPSCRIPT_found_E4ZXH9LAH07QF1RNOKDL=1"
    )
    if "!TMPSCRIPT_found_E4ZXH9LAH07QF1RNOKDL!"=="0" (
        echo Invalid key in config^(ie. not in whitelist^): "!TMP_key_N8RG305DWBRF52TCWV41!"
        pause
        exit /b 1
    )
    
    rem if it's empty it's cleared because set "VAR=" removes it, so... store it anyway:
    rem store trimmed value in temporary variable (same name as key)
    set "TMP_!TMP_key_N8RG305DWBRF52TCWV41!_N8RG305DWBRF52TCWV41=!TMP_val_N8RG305DWBRF52TCWV41!"
)

::specialized check(s) for some args
if not defined TMP_exe_name_N8RG305DWBRF52TCWV41 (
    echo Missing 'exe_name' entry in config file "%TMPSCRIPT_cfgfname_E4ZXH9LAH07QF1RNOKDL%" at location "%TMPSCRIPT_cfg_E4ZXH9LAH07QF1RNOKDL%"
    pause
    exit /b 1
)
if /i not "%TMP_exe_name_N8RG305DWBRF52TCWV41:~-4%"==".exe" (
    echo Config error: "%TMP_exe_name_N8RG305DWBRF52TCWV41%" is not an .exe
    pause
    exit /b 1
)

::export the read vars to be available in the caller
for %%W in (!TMPSCRIPT_WHITELIST_E4ZXH9LAH07QF1RNOKDL!) do (
    if defined %%W (
        echo Error: variable "%%W" already defined in environment — aborting.
        pause
        exit /b 1
    )
    if defined TMP_%%W_N8RG305DWBRF52TCWV41 (
        set "%%W=!TMP_%%W_N8RG305DWBRF52TCWV41!"
    )
)

::cleanup our temp vars and the read temp vars
for /f "tokens=1 delims==" %%V in ('set 2^>nul ^| findstr /r /b /i /c:"TMP_.*_N8RG305DWBRF52TCWV41" /c:"TMPSCRIPT_.*_E4ZXH9LAH07QF1RNOKDL"') do set "%%V="

rem don't endlocal or we lose everything we read/set! But here’s the critical point: when the batch file exits, CMD automatically discards all active setlocal environments, regardless of whether you explicitly ran endlocal or not.

if "%READCFG_PRIME%" == "DEBUG" (
  rem set | find "TMP"
  set
  echo DEBUG is all done.
  pause
)
exit /b

goto :after_trim

:trim
setlocal EnableDelayedExpansion
set "s=%~1"

:: Trim leading spaces
for /f "tokens=* delims= " %%T in ("!s!") do set "s=%%T"

:: Trim trailing spaces
:trim_tail
if defined s (
    if "!s:~-1!"==" " (
        set "s=!s:~0,-1!"
        goto trim_tail
    )
)

endlocal & set "%~2=%s%"
exit /b

:after_trim