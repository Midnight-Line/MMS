@echo off
rem Stops every process the test bed starts. The window titles match the ones
rem used by run-all.cmd and the start-*.cmd scripts.
setlocal

for %%T in (
    mms-router
    mms-shore-er
    mms-vde-er
    mms-vessel-er-01
    mms-vessel-er-02
    mms-corenet
) do (
    taskkill /fi "WINDOWTITLE eq %%T*" /t /f >nul 2>&1
)

rem The fake ship modems run in windows named after their MMSI.
for %%T in (mms-modem-*) do (
    taskkill /fi "WINDOWTITLE eq %%T*" /t /f >nul 2>&1
)

rem Anything left over that holds one of the test bed ports.
for %%P in (8080 8888 8890 8896 8898 9000 16030 16031) do (
    for /f "tokens=5" %%A in ('netstat -ano ^| findstr /r /c:":%%P .*LISTENING"') do (
        taskkill /pid %%A /t /f >nul 2>&1
    )
)

echo Test bed stopped.
endlocal
