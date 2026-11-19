@echo off
rem Shore side test flow: an agent on the shore Edge Router publishes an alert on
rem the vessel-alerts subject. The VDE-TER gateway is subscribed to that subject
rem on the MMS Router, so the message leaves the IP network at the gateway and
rem continues over the VDE link as a coverage broadcast to both ship modems.
setlocal
call "%~dp0env.cmd"

if "%~1"=="" (
    set "BODY=alert: gale warning in the approaches"
) else (
    set "BODY=%~1"
)

"%BIN_DIR%\testagent.exe" ^
    -er "wss://127.0.0.1:%SHORE_ER_PORT%" ^
    -cert "%CERT_DIR%\shore-cmd-01.crt" ^
    -key "%CERT_DIR%\shore-cmd-01.key" ^
    -ca "%CA_FILE%" ^
    -subscribe "%UPLINK_SUBJECT%" ^
    -send-subject "%DOWNLINK_SUBJECT%" ^
    -send "%BODY%" ^
    -listen 10s
endlocal
