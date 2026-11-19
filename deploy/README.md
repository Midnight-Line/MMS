# MMS / VDE test deployment

A self-contained local deployment of the Maritime Messaging Service with VDE
transport support. It brings up every component of the architecture on a single
machine so that each role can be exercised on its own or as part of the full
end-to-end chain.

## Roles

| Role | Binary / script | What it is |
| --- | --- | --- |
| MMS Router | `bin/router.exe` (`start-router.cmd`) | The message broker. Edge Routers connect to it over TLS WebSocket and it forwards messages between them by subject or by recipient MRN. |
| Shore Edge Router | `bin/edgerouter.exe` (`start-shore-er.cmd`) | An Edge Router on the shore side. Agents connect to it over TLS WebSocket. IP-only transport in this deployment. |
| VDE-ER (VDE-TER gateway) | `bin/edgerouter.exe` + `fake_corenet.py` (`start-vde-er.cmd`) | The VDE-TER gateway. It is an Edge Router running in `shore` VDE mode: it attaches to the VDE core network over TCP and relays application messages between the MMS Router (IP) and the VDE side. `fake_corenet.py` stands in for the real VDE core network. |
| Ship Edge Router 1 | `bin/edgerouter.exe` + `fake_ship_modem.py` (`start-vessel-er-01.cmd`) | An Edge Router running in `ship` VDE mode. Its downstream is a ship modem on the PI serial/TCP interface instead of the MMS Router. |
| Ship Edge Router 2 | same, port/MMSI 2 (`start-vessel-er-02.cmd`) | Second ship, used to verify broadcast and multi-recipient delivery. |

Two supporting pieces:

* `fake_corenet.py` — a stand-in for the VDE core network. Speaks the
  newline-delimited JSON `uplink` / `downlink` / `ack` protocol on a TCP port and
  routes traffic between the gateway and the registered ship modems.
* `fake_ship_modem.py` — a stand-in for a VDE ship modem. On one side it is a PI
  server (`$VDTDB`, `$VDTSB`, `$VDEDM`, `$VDESI`, `$VDEDO` sentences) for the ship
  Edge Router; on the other it is a radio peer of the fake core network.

## Ports

Everything is configurable in `env.cmd`. The defaults avoid the ports the MMS
Router and Prometheus use by default (8080, 8081, 2113, 27000), which are often
already taken on a developer machine.

| Component | Service port | Prometheus | Health |
| --- | --- | --- | --- |
| MMS Router | 18080 | 12113 | 18081 |
| Shore ER | 18888 | 12112 | 18889 |
| VDE-ER gateway | 18890 | 12115 | 18891 |
| Ship ER 1 | 18898 | 12114 | 18899 |
| Ship ER 2 | 18896 | 12116 | 18897 |
| Fake core network | 19000 | — | — |
| Ship modem 1 (PI) | 16030 | — | — |
| Ship modem 2 (PI) | 16031 | — | — |
| Router libp2p | 27001 | — | — |

## Quick start

```cmd
cd deploy
build.cmd
prepare-certs.cmd
run-all.cmd
```

Then send a message from the shore and watch both ships receive it:

```cmd
send-alert.cmd "COLREG warning: crossing traffic"
```

And send a report from a ship back to the shore:

```cmd
send-report.cmd 1 "Position report: lat 22.54 lon 114.06"
```

Stop everything with:

```cmd
stop-all.cmd
```

## Certificates

All connections are mutually authenticated with MCP certificates. The MRN of an
Edge Router is taken from the UID field of its client certificate, and the MMS
Router rejects any Edge Router whose certificate does not match the MRN in its
`Connect` message.

`prepare-certs.cmd` copies the certificates from `D:\maphao\mcp-certs` into
`deploy\certs` and creates the CA chain file. If `vde-ter-er-01` is not present
it is issued on the spot by `certgen`, using the identity registry sub-CA in
`deploy\secrets\subca-legacy.p12`.

To issue additional certificates:

```cmd
bin\certgen.exe -subca secrets\subca-legacy.p12 -subca-pass <password> ^
    -name vessel-er-03 -kind er -out certs
```

`-kind er` issues a certificate with both `serverAuth` and `clientAuth`, which
is what an Edge Router needs. `-kind agent` issues `clientAuth` only, which is
what an agent connecting to an Edge Router needs. The MRN defaults to
`urn:mrn:mcp:mms:idp1:bootstrap:<name>` for Edge Routers and
`urn:mrn:mcp:entity:idp1:bootstrap:<name>` for agents; use `-mrn` to override.

Note that the certificates in `mcp-certs` carry no OCSP or CRL endpoint, so every
component is started with `-skip-revocation-check`. Remove that flag once the
certificates come from a registry that publishes revocation information.

## Testing individual roles

Each role can be started on its own; they do not require the others to be up.

**Router only**

```cmd
start-router.cmd
```

The Router needs a beacons file (`deploy\beacons.txt`) even when it is the only
Router in the network; libp2p reads it during peer discovery.

**Shore Edge Router only**

```cmd
start-shore-er.cmd
```

Without a Router it still serves agents on 18888. Sending to a subject with no
subscribers simply logs that no subscriber was found. Note that the Edge Router
retries its Router connection every five seconds, so the first `Router connected`
line can take a few seconds to appear.

**VDE-ER gateway only**

```cmd
start-vde-er.cmd
```

This starts the fake core network and then the gateway. The gateway logs
`VDE core network link up` once the TCP connection to the core network is
established. With `-vde-relay` it subscribes to `vessel-alerts` and to both
vessel agent MRNs on the MMS Router, so messages addressed to those reach the
VDE side.

**Ship Edge Router only**

```cmd
start-vessel-er-01.cmd
```

This starts the fake ship modem and then the ship Edge Router. The Edge Router
logs `VDE ship modem link up` once the PI connection is up. Sending to a
recipient whose MRN is not in the `-vde-mrn-mmsi` mapping fails with
`no MMSI mapping known for any recipient` and the message is dropped rather
than retried, which is what you want: the mapping has to be configured before
the link can be used.

**Agents**

`bin/testagent.exe` is a small MMTP client used to drive the tests:

```cmd
bin\testagent.exe -er wss://127.0.0.1:18888 ^
    -cert certs\shore-cmd-01.crt -key certs\shore-cmd-01.key ^
    -ca certs\mcp-ca-chain.pem ^
    -subscribe shore-reports ^
    -send-subject vessel-alerts -send "hello ships" ^
    -listen 30s
```

It connects, authenticates with its certificate, optionally subscribes to a
subject or to direct messages, optionally sends one message, and then prints
every message it is notified about until the listen window expires.

## Message flow

A shore-to-ship alert travels like this:

```
shore agent
  -> Shore ER (18888)            TLS WebSocket, MMTP SEND
  -> MMS Router (18080)          forwarded to subscribers of "vessel-alerts"
  -> VDE-ER gateway (18890)      subscribed to "vessel-alerts"; relays over VDE
  -> fake core network (19000)   downlink, broadcast (MMSI 0)
  -> ship modems (16030/16031)   PI: $VDEDM + $VDESI sentences
  -> Ship ERs (18898/18896)      MMTP SEND, queued for local subscribers
  -> ship agents                 NOTIFY, then RECEIVE
```

A ship-to-shore report travels the reverse way: ship agent → Ship ER → PI TDB
fragments → ship modem → core network `uplink` → VDE-ER gateway → MMS Router →
Shore ER → shore agent.

## Notes on the VDE side

* The VDE core network interface carries one JSON object per line over TCP. The
  only message types are `uplink`, `downlink` and `ack`. Anything else — `ping`
  and `coverage` in the current implementation — is an extension that a real
  core network is free to ignore.
* `uplink.data` is a base64-encoded, already reassembled MMTP message: the core
  network does the reassembly, not the Edge Router. Conversely `downlink.data`
  is handed to the core network whole and the core network fragments it.
* On the PI interface the Edge Router sends `$VDTDB` sentences for long messages
  and `$VDTSB` for short ones, and receives `$VDEDM` (with a following `$VDESI`
  carrying the sender's MMSI), `$VDTDB` and `$VDTSB`.
* A message that fits in a single TDB sentence is sent as a lone Start fragment
  with no End fragment, so both the Edge Router and the fake modem treat
  `TotalSentences <= 1` as a complete message rather than waiting for a
  terminator that will never arrive.
* The MRN-to-MMSI mapping is configured with `-vde-mrn-mmsi` on both sides.
  Entries given on the command line are authoritative and never expire; entries
  learned from uplink traffic expire after `-vde-map-ttl` (15 minutes by
  default).
* Link selection is controlled by `-vde-preferred`:
  * `ip` — use the MMS Router whenever it is reachable.
  * `vde` — use the VDE link whenever it is reachable.
  * `auto` — use VDE for messages up to 8192 bytes and IP for larger ones, and
    additionally mirror the message on the other link when it is healthy. The
    duplicate is suppressed on the receiving side by a content fingerprint.

## Troubleshooting

**The Router takes about 20 seconds to listen.** libp2p spends roughly 20
seconds searching for peers before it starts the WebSocket listener. This is
expected; watch for `Websocket listening on: :18080`.

**`no MMSI mapping known for any recipient`** — the destination MRN has no entry
in `-vde-mrn-mmsi` on the sending Edge Router. Add it, or send once from that
recipient so the mapping is learned from its uplink.

**Messages are delivered twice** — this was a real defect, fixed by fingerprint
based duplicate suppression in the Edge Router. If it reappears, check that
`queueApplicationMessage` is returning `handled=true` for the second copy.

**Nothing arrives at the ships** — check, in order: the gateway logs
`Relayed message ... over VDE`; the core network logs a `downlink` line; the
ship modem logs `delivered downlink ... to the ship`; the ship Edge Router logs
the notification. The first stage that stays silent identifies the broken hop.

**Agents cannot connect** — the Edge Router verifies that the MRN in the
`Connect` message matches the UID in the client certificate. A mismatch closes
the connection with a `MrnMismatchErr`. Also check that the agent trusts the CA
chain passed with `-ca`.

**Ports are already in use** — change them in `env.cmd`. All scripts read their
ports from there, so a single edit moves the whole deployment.
