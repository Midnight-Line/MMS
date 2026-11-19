@echo off
rem Starts ship Edge Router #2 together with the fake ship modem that serves it.
setlocal
call "%~dp0env.cmd"

if not exist "%CERT_DIR%\vessel-er-02.crt" (
    echo Missing %CERT_DIR%\vessel-er-02.crt
    exit /b 1
)

echo Starting fake ship modem for MMSI %VESSEL2_MMSI% on port %VESSEL2_MODEM_PORT%
start "mms-modem-%VESSEL2_MMSI%" /min cmd /c python "%DEPLOY_DIR%fake_ship_modem.py" --port %VESSEL2_MODEM_PORT% --mmsi %VESSEL2_MMSI% --core-port %CORENET_PORT% -v

timeout /t 2 /nobreak >nul

echo Starting ship Edge Router %VESSEL2_ER_MRN% on port %VESSEL2_ER_PORT%
"%BIN_DIR%\edgerouter.exe" ^
    -raddr "%ROUTER_URL%" ^
    -port %VESSEL2_ER_PORT% ^
    -cert-path "%CERT_DIR%\vessel-er-02.crt" ^
    -cert-key-path "%CERT_DIR%\vessel-er-02.key" ^
    -client-cert "%CERT_DIR%\vessel-er-02.crt" ^
    -client-cert-key "%CERT_DIR%\vessel-er-02.key" ^
    -client-ca "%CA_FILE%" ^
    -tlsca "%CA_FILE%" ^
    -prometheus-port %VESSEL2_ER_PROM_PORT% ^
    -health-port %VESSEL2_ER_HEALTH_PORT% ^
    -vde-enabled ^
    -vde-mode ship ^
    -vde-modem-addr 127.0.0.1:%VESSEL2_MODEM_PORT% ^
    -vde-preferred vde ^
    -vde-mrn-mmsi "%SHORE_AGENT_MRN%=%SHORE_MMSI%" ^
    -skip-revocation-check ^
    %*
endlocal
