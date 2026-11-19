@echo off
rem Shared configuration for the MMS deploy test bed.
rem
rem Every value below can be overridden by setting the variable before a script
rem is called, which is handy when the certificates live somewhere else.
rem
rem The ports avoid the ranges that are commonly taken on a developer machine
rem (8080 and 27000 in particular are often already in use).

set "DEPLOY_DIR=%~dp0"
for %%I in ("%DEPLOY_DIR%..") do set "MMS_HOME=%%~fI"

set "BIN_DIR=%DEPLOY_DIR%bin"
set "RUN_DIR=%DEPLOY_DIR%run"
set "LOG_DIR=%RUN_DIR%\logs"

if not defined CERT_DIR set "CERT_DIR=%DEPLOY_DIR%certs"
if not defined CA_FILE set "CA_FILE=%CERT_DIR%\mcp-ca-chain.pem"

rem -- ports -------------------------------------------------------------
if not defined ROUTER_PORT set "ROUTER_PORT=18080"
if not defined ROUTER_LIBP2P_PORT set "ROUTER_LIBP2P_PORT=27001"
if not defined ROUTER_PROM_PORT set "ROUTER_PROM_PORT=12113"
if not defined ROUTER_HEALTH_PORT set "ROUTER_HEALTH_PORT=18081"

if not defined SHORE_ER_PORT set "SHORE_ER_PORT=18888"
if not defined SHORE_ER_PROM_PORT set "SHORE_ER_PROM_PORT=12112"
if not defined SHORE_ER_HEALTH_PORT set "SHORE_ER_HEALTH_PORT=18889"

if not defined GW_ER_PORT set "GW_ER_PORT=18890"
if not defined GW_ER_PROM_PORT set "GW_ER_PROM_PORT=12115"
if not defined GW_ER_HEALTH_PORT set "GW_ER_HEALTH_PORT=18891"

if not defined VESSEL1_ER_PORT set "VESSEL1_ER_PORT=18898"
if not defined VESSEL1_ER_PROM_PORT set "VESSEL1_ER_PROM_PORT=12114"
if not defined VESSEL1_ER_HEALTH_PORT set "VESSEL1_ER_HEALTH_PORT=18899"
if not defined VESSEL1_MODEM_PORT set "VESSEL1_MODEM_PORT=16030"
if not defined VESSEL1_MMSI set "VESSEL1_MMSI=992359598"

if not defined VESSEL2_ER_PORT set "VESSEL2_ER_PORT=18896"
if not defined VESSEL2_ER_PROM_PORT set "VESSEL2_ER_PROM_PORT=12116"
if not defined VESSEL2_ER_HEALTH_PORT set "VESSEL2_ER_HEALTH_PORT=18897"
if not defined VESSEL2_MODEM_PORT set "VESSEL2_MODEM_PORT=16031"
if not defined VESSEL2_MMSI set "VESSEL2_MMSI=992359599"

if not defined CORENET_PORT set "CORENET_PORT=19000"

rem -- identities --------------------------------------------------------
rem The MRN of an Edge Router is taken from the UID of its client certificate,
rem so these only have to match the certificates that are actually used.
if not defined ROUTER_MRN set "ROUTER_MRN=urn:mrn:mcp:mms:idp1:bootstrap:router-local"
if not defined SHORE_ER_MRN set "SHORE_ER_MRN=urn:mrn:mcp:mms:idp1:bootstrap:shantou-er-01"
if not defined GW_ER_MRN set "GW_ER_MRN=urn:mrn:mcp:mms:idp1:bootstrap:vde-ter-er-01"
if not defined VESSEL1_ER_MRN set "VESSEL1_ER_MRN=urn:mrn:mcp:mms:idp1:bootstrap:vessel-er-01"
if not defined VESSEL2_ER_MRN set "VESSEL2_ER_MRN=urn:mrn:mcp:mms:idp1:bootstrap:vessel-er-02"

if not defined SHORE_AGENT_MRN set "SHORE_AGENT_MRN=urn:mrn:mcp:entity:idp1:bootstrap:shore-cmd-01"
if not defined VESSEL1_AGENT_MRN set "VESSEL1_AGENT_MRN=urn:mrn:mcp:entity:idp1:bootstrap:vessel-rx-01"
if not defined VESSEL2_AGENT_MRN set "VESSEL2_AGENT_MRN=urn:mrn:mcp:entity:idp1:bootstrap:vessel-rx-02"

rem MMSI of the station that fronts each MRN over the VDE air interface.
rem Agents are reachable through the MMSI of the Edge Router that serves them.
if not defined SHORE_MMSI set "SHORE_MMSI=992359500"
if not defined GW_MMSI set "GW_MMSI=992359000"

rem -- topics used by the sample flows -----------------------------------
if not defined DOWNLINK_SUBJECT set "DOWNLINK_SUBJECT=vessel-alerts"
if not defined UPLINK_SUBJECT set "UPLINK_SUBJECT=shore-reports"

set "ROUTER_URL=wss://127.0.0.1:%ROUTER_PORT%"

if not exist "%BIN_DIR%" mkdir "%BIN_DIR%"
if not exist "%LOG_DIR%" mkdir "%LOG_DIR%"
