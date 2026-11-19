@echo off
rem Ship side test flow: an agent on one of the ship Edge Routers publishes a
rem report on the shore-reports subject. The ship Edge Router prefers the VDE
rem link, so the message travels over the PI link to the fake ship modem, through
rem the fake core network to the VDE-TER gateway, and from there into the MMS
rem Router, where the shore agent picks it up.
rem
rem Usage: send-report.cmd [1^|2] ["body"]
setlocal
call "%~dp0env.cmd"

set "VESSEL=%~1"
if "%VESSEL%"=="" set "VESSEL=1"

if "%VESSEL%"=="1" (
    set "ER_PORT=%VESSEL1_ER_PORT%"
    set "AGENT=vessel-rx-01"
) else (
    set "ER_PORT=%VESSEL2_ER_PORT%"
    set "AGENT=vessel-rx-02"
)

if "%~2"=="" (
    set "BODY=report: all systems nominal"
) else (
    set "BODY=%~2"
)

"%BIN_DIR%\testagent.exe" ^
    -er "wss://127.0.0.1:%ER_PORT%" ^
    -cert "%CERT_DIR%\%AGENT%.crt" ^
    -key "%CERT_DIR%\%AGENT%.key" ^
    -ca "%CA_FILE%" ^
    -subscribe "%DOWNLINK_SUBJECT%" ^
    -send-subject "%UPLINK_SUBJECT%" ^
    -send "%BODY%" ^
    -listen 10s
endlocal
