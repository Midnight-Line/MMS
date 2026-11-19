@echo off
rem Copies the certificates the test bed needs into deploy\certs and issues the
rem one certificate that does not exist yet: the VDE-TER gateway Edge Router.
rem
rem Override CERT_SOURCE if your certificates live somewhere else.
setlocal
call "%~dp0env.cmd"

if not defined CERT_SOURCE set "CERT_SOURCE=D:\maphao\mcp-certs"
if not defined CA_SOURCE set "CA_SOURCE=D:\maphao\Workspace\JavaProject\mcp-testbed\IdentityRegistry\mcp-ca-chain.pem"
if not defined SUBCA_P12 set "SUBCA_P12=%DEPLOY_DIR%secrets\subca-legacy.p12"
if not defined SUBCA_PASS set "SUBCA_PASS=changeit"

if not exist "%CERT_DIR%" mkdir "%CERT_DIR%"

echo Copying certificates from %CERT_SOURCE%
for %%C in (
    router-local
    er-shantou-er-01
    vessel-er-01
    vessel-er-02
    shore-cmd-01
    vessel-rx-01
    vessel-rx-02
    secom-01
) do (
    if exist "%CERT_SOURCE%\%%C.crt" (
        copy /y "%CERT_SOURCE%\%%C.crt" "%CERT_DIR%\" >nul
        copy /y "%CERT_SOURCE%\%%C.key" "%CERT_DIR%\" >nul
    ) else (
        echo   missing %%C in %CERT_SOURCE%
    )
)

if exist "%CA_SOURCE%" (
    copy /y "%CA_SOURCE%" "%CA_FILE%" >nul
) else (
    echo Missing CA chain %CA_SOURCE%
    exit /b 1
)

if not exist "%CERT_DIR%\vde-ter-er-01.crt" (
    if exist "%SUBCA_P12%" (
        echo Issuing vde-ter-er-01 from %SUBCA_P12%
        "%BIN_DIR%\certgen.exe" -subca "%SUBCA_P12%" -subca-pass "%SUBCA_PASS%" ^
            -name vde-ter-er-01 -kind er -out "%CERT_DIR%" || exit /b 1
    ) else (
        echo.
        echo Missing %SUBCA_P12%, so vde-ter-er-01 cannot be issued.
        echo Export the Identity Registry sub-CA key first:
        echo   keytool -importkeystore -srckeystore ^<ca-keystore^>.jks -srcstoretype JKS ^
        echo     -srcstorepass ^<pass^> -srcalias urn:mrn:mcp:ca:idp1:mcp-idreg -srckeypass ^<pass^> ^
        echo     -destkeystore "%SUBCA_P12%" -deststoretype PKCS12 -deststorepass ^<pass^> ^
        echo     -J-Dkeystore.pkcs12.keyProtectionAlgorithm=PBEWithSHA1AndDESede ^
        echo     -J-Dkeystore.pkcs12.certProtectionAlgorithm=PBEWithSHA1AndRC2_40 ^
        echo     -J-Dkeystore.pkcs12.macAlgorithm=HmacPBESHA1
        exit /b 1
    )
)

echo.
echo Certificates in %CERT_DIR%:
dir /b "%CERT_DIR%"
endlocal
