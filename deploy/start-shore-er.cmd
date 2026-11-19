@echo off
rem Starts the shore Edge Router. It only uses the IP transport and connects to
rem the MMS Router, so it can be run on its own as soon as the Router is up.
setlocal
call "%~dp0env.cmd"

echo Starting shore Edge Router %SHORE_ER_MRN% on port %SHORE_ER_PORT%
"%BIN_DIR%\edgerouter.exe" ^
    -raddr "%ROUTER_URL%" ^
    -port %SHORE_ER_PORT% ^
    -cert-path "%CERT_DIR%\er-shantou-er-01.crt" ^
    -cert-key-path "%CERT_DIR%\er-shantou-er-01.key" ^
    -client-cert "%CERT_DIR%\er-shantou-er-01.crt" ^
    -client-cert-key "%CERT_DIR%\er-shantou-er-01.key" ^
    -client-ca "%CA_FILE%" ^
    -tlsca "%CA_FILE%" ^
    -prometheus-port %SHORE_ER_PROM_PORT% ^
    -health-port %SHORE_ER_HEALTH_PORT% ^
    -skip-revocation-check ^
    %*
endlocal
