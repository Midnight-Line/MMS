@echo off
rem Starts the MMS Router. It terminates the TLS websocket connections of every
rem Edge Router and forwards messages between them.
setlocal
call "%~dp0env.cmd"

if not exist "%DEPLOY_DIR%beacons.txt" (
    copy /y "%MMS_HOME%\router\beacons.txt" "%DEPLOY_DIR%beacons.txt" >nul
)

if not exist "%CERT_DIR%\router-local.crt" (
    echo Missing %CERT_DIR%\router-local.crt
    exit /b 1
)

echo Starting MMS Router on port %ROUTER_PORT% (libp2p %ROUTER_LIBP2P_PORT%)
"%BIN_DIR%\router.exe" ^
    -port %ROUTER_PORT% ^
    -libp2p-port %ROUTER_LIBP2P_PORT% ^
    -beacons "%DEPLOY_DIR%beacons.txt" ^
    -cert-path "%CERT_DIR%\router-local.crt" ^
    -cert-key-path "%CERT_DIR%\router-local.key" ^
    -client-ca "%CA_FILE%" ^
    -prometheus-port %ROUTER_PROM_PORT% ^
    -health-port %ROUTER_HEALTH_PORT% ^
    -skip-revocation-check ^
    -l DNK ^
    %*
endlocal
