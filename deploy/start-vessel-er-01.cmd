@echo off
rem Starts ship Edge Router #1 together with the fake ship modem that serves it.
rem
rem The Edge Router keeps its IP connection to the MMS Router for the control
rem plane (connect, subscribe, receive) and uses the VDE presentation interface
rem for application messages, which is what a ship at sea with no backhaul ends
rem up doing.
setlocal
call "%~dp0env.cmd"

if not exist "%CERT_DIR%\vessel-er-01.crt" (
    echo Missing %CERT_DIR%\vessel-er-01.crt
    exit /b 1
)

echo Starting fake ship modem for MMSI %VESSEL1_MMSI% on port %VESSEL1_MODEM_PORT%
start "mms-modem-%VESSEL1_MMSI%" /min cmd /c python "%DEPLOY_DIR%fake_ship_modem.py" --port %VESSEL1_MODEM_PORT% --mmsi %VESSEL1_MMSI% --core-port %CORENET_PORT% -v

timeout /t 2 /nobreak >nul

echo Starting ship Edge Router %VESSEL1_ER_MRN% on port %VESSEL1_ER_PORT%
"%BIN_DIR%\edgerouter.exe" ^
    -raddr "%ROUTER_URL%" ^
    -port %VESSEL1_ER_PORT% ^
    -cert-path "%CERT_DIR%\vessel-er-01.crt" ^
    -cert-key-path "%CERT_DIR%\vessel-er-01.key" ^
    -client-cert "%CERT_DIR%\vessel-er-01.crt" ^
    -client-cert-key "%CERT_DIR%\vessel-er-01.key" ^
    -client-ca "%CA_FILE%" ^
    -tlsca "%CA_FILE%" ^
    -prometheus-port %VESSEL1_ER_PROM_PORT% ^
    -health-port %VESSEL1_ER_HEALTH_PORT% ^
    -vde-enabled ^
    -vde-mode ship ^
    -vde-modem-addr 127.0.0.1:%VESSEL1_MODEM_PORT% ^
    -vde-preferred vde ^
    -vde-mrn-mmsi "%SHORE_AGENT_MRN%=%SHORE_MMSI%" ^
    -skip-revocation-check ^
    %*
endlocal
