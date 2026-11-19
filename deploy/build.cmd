@echo off
rem Builds every binary the test bed needs into deploy\bin.
rem
rem The Edge Router package is built from the package directory (not from the
rem single edgerouter.go file) because the VDE transport lives in sibling files
rem such as vdelink.go and vdemodem.go.
setlocal
call "%~dp0env.cmd"

if not exist "%BIN_DIR%" mkdir "%BIN_DIR%"

echo Building router.exe
pushd "%MMS_HOME%\router"
go build -o "%BIN_DIR%\router.exe" .
if errorlevel 1 exit /b 1
popd

echo Building edgerouter.exe
pushd "%MMS_HOME%\edgerouter"
go build -o "%BIN_DIR%\edgerouter.exe" .
if errorlevel 1 exit /b 1
popd

echo Building testagent.exe
pushd "%DEPLOY_DIR%testagent"
go build -o "%BIN_DIR%\testagent.exe" .
if errorlevel 1 exit /b 1
popd

echo Building certgen.exe
pushd "%DEPLOY_DIR%certgen"
set "GOFLAGS=-mod=mod"
set "GOPROXY=off"
go build -o "%BIN_DIR%\certgen.exe" .
if errorlevel 1 exit /b 1
popd

if not exist "%DEPLOY_DIR%beacons.txt" (
    copy /y "%MMS_HOME%\router\beacons.txt" "%DEPLOY_DIR%beacons.txt" >nul
)

echo.
dir /b "%BIN_DIR%"
endlocal
