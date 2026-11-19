@echo off
rem Starts the VDE-TER gateway: the fake VDE core network plus one Edge Router
rem that bridges between the MMS Router (IP) and the VDE air interface.
rem
rem This is the combination that a real deployment splits between the VDE core
rem network operator and the gateway Edge Router; here both halves run locally
rem so the whole path can be exercised without any VDE equipment.
setlocal
call "%~dp0env.cmd"

if not exist "%CERT_DIR%\vde-ter-er-01.crt" (
    echo Missing %CERT_DIR%\vde-ter-er-01.crt
    echo Issue it with: certgen -subca ^<subca.p12^> -name vde-ter-er-01 -kind er -out "%CERT_DIR%"
    exit /b 1
)

echo Starting fake VDE core network on port %CORENET_PORT%
start "mms-corenet" /min cmd /c python "%DEPLOY_DIR%fake_corenet.py" --port %CORENET_PORT% -v

rem Give the core network a moment to bind its port before the gateway dials it.
timeout /t 2 /nobreak >nul

echo Starting VDE-TER gateway Edge Router %GW_ER_MRN% on port %GW_ER_PORT%
"%BIN_DIR%\edgerouter.exe" ^
    -raddr "%ROUTER_URL%" ^
    -port %GW_ER_PORT% ^
    -cert-path "%CERT_DIR%\vde-ter-er-01.crt" ^
    -cert-key-path "%CERT_DIR%\vde-ter-er-01.key" ^
    -client-cert "%CERT_DIR%\vde-ter-er-01.crt" ^
    -client-cert-key "%CERT_DIR%\vde-ter-er-01.key" ^
    -client-ca "%CA_FILE%" ^
    -tlsca "%CA_FILE%" ^
    -prometheus-port %GW_ER_PROM_PORT% ^
    -health-port %GW_ER_HEALTH_PORT% ^
    -vde-enabled ^
    -vde-mode shore ^
    -vde-core-addr 127.0.0.1:%CORENET_PORT% ^
    -vde-preferred vde ^
    -vde-relay ^
    -vde-relay-subjects "%DOWNLINK_SUBJECT%,%VESSEL1_AGENT_MRN%,%VESSEL2_AGENT_MRN%" ^
    -vde-mrn-mmsi "%VESSEL1_AGENT_MRN%=%VESSEL1_MMSI%,%VESSEL2_AGENT_MRN%=%VESSEL2_MMSI%" ^
    -skip-revocation-check ^
    %*
endlocal
