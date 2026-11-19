@echo off
rem Brings up the whole MMS test bed in the right order:
rem   MMS Router -> shore Edge Router -> VDE-TER gateway (fake core network +
rem   gateway Edge Router) -> two ship Edge Routers with fake ship modems.
rem
rem Each role gets its own window so that its log stays readable. Use
rem stop-all.cmd to tear everything down again.
setlocal
call "%~dp0env.cmd"

if not exist "%BIN_DIR%\router.exe" (
    echo Binaries are missing, run build.cmd first
    exit /b 1
)
if not exist "%CERT_DIR%\router-local.crt" (
    echo Certificates are missing, run prepare-certs.cmd first
    exit /b 1
)

echo [1/5] MMS Router on port %ROUTER_PORT%
start "mms-router" cmd /c ""%DEPLOY_DIR%start-router.cmd""
timeout /t 3 /nobreak >nul

echo [2/5] Shore Edge Router on port %SHORE_ER_PORT%
start "mms-shore-er" cmd /c ""%DEPLOY_DIR%start-shore-er.cmd""
timeout /t 2 /nobreak >nul

echo [3/5] VDE-TER gateway on port %GW_ER_PORT% with core network on %CORENET_PORT%
start "mms-vde-er" cmd /c ""%DEPLOY_DIR%start-vde-er.cmd""
timeout /t 3 /nobreak >nul

echo [4/5] Ship Edge Router 1 on port %VESSEL1_ER_PORT%
start "mms-vessel-er-01" cmd /c ""%DEPLOY_DIR%start-vessel-er-01.cmd""
timeout /t 2 /nobreak >nul

echo [5/5] Ship Edge Router 2 on port %VESSEL2_ER_PORT%
start "mms-vessel-er-02" cmd /c ""%DEPLOY_DIR%start-vessel-er-02.cmd""
timeout /t 2 /nobreak >nul

echo.
echo Test bed started. Roles:
echo   MMS Router        wss://127.0.0.1:%ROUTER_PORT%   health :%ROUTER_HEALTH_PORT%
echo   Shore ER          wss://127.0.0.1:%SHORE_ER_PORT%   health :%SHORE_ER_HEALTH_PORT%
echo   VDE-TER gateway   wss://127.0.0.1:%GW_ER_PORT%   health :%GW_ER_HEALTH_PORT%
echo   Ship ER 1         wss://127.0.0.1:%VESSEL1_ER_PORT%   health :%VESSEL1_ER_HEALTH_PORT%
echo   Ship ER 2         wss://127.0.0.1:%VESSEL2_ER_PORT%   health :%VESSEL2_ER_HEALTH_PORT%
echo.
echo Use send-alert.cmd and send-report.cmd to push messages through the bed.
endlocal
