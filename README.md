# Maritime Messaging Service (MMS)

This is an implementation of the Maritime Messaging Service - One of the three core components in
the [Maritime Connectivity Platform (MCP)](https://maritimeconnectivity.net/mcp-documents/). It is under the Apache 2.0
License.

The following sections describe how to set up the router and edge router components of the MMS. The edge router can
optionally carry MMS traffic over VDES, see [VDE transport](#vde-transport).

For instructions on how to set up an MMS Agent, read [here](#setting-up-an-mms-agent)

**DISCLAIMER:** This code is provided "as is" and without warranties of any kind, either express or implied. It is a
work in progress and may contain bugs, errors, or inconsistencies that could cause system failures or discrepancies in
data. By using this code, you acknowledge and agree that you do so at your own risk. The developers make no guarantees
regarding the functionality, reliability, or safety of the code, and will not be liable for any damages or losses
incurred from its use.

## Prerequisites

* Go version 1.26 or higher

## Build

A Makefile has been provided in the root directory.

* Build all targets at once with: `Make all`
* All executables will be placed in the `/bin` directory
* *Alternatively navigate to each directory in turn, and build the executables independently
  using `go build <target.go>`*

## Configurations

### Certificates (Required for TLS)

The proper certificates are required for router and edgerouter operation with TLS. Note that the MMS protocol
specification mandates the use TLS for the connections between components.

#### Edgerouter certificates:

* An MCP certificate containing the Edgerouter MRN, including the private key
* A TLS certificate, including the private key

#### Router certificates

* A private key for libp2p for known router identities within
  the [libp2p](https://docs.libp2p.io/concepts/fundamentals/protocols/) router p2p-network.
* A TLS certificate, including the private key

### Beacons

A `beacons.txt` file has been provided as a convenience for bootstrapping a libp2p-network. When starting an MMS router,
it will attempt to
connect to router identities specified in the `beacons.txt`

* Example `/ip4/127.0.0.1/udp/27000/quic-v1/p2p/QmcUKyMuepvXqZhpMSBP59KKBymRNstk41qGMPj38QStfx` specifies a localhost
  router, listening on the QUIC protocol on UDP port 27000. It has the libp2p-identity
  `QmcUKyMuepvXqZhpMSBP59KKBymRNstk41qGMPj38QStfx`, which is derived from the private key.

### Calculating a key id

A small utility is provided to simplify calculating the key id for `beacons.txt`:

* `./bin/genkey -privkey path/to/private/key.pem`

## How to run

### Edgerouter

The followings flags can be provided to the MMS edgerouter. Note that not specifying the proper certificates imposes
restrictions on the allowed operations.

#### Flags

* `raddr`  The websocket URL of the Router to connect to.
* `port` The port number that this edgerouter should listen on. Agents shall use this port to connect to the edgerouter.
* `mrn` The MRN of this Edge Router
* `client-cert` Path to the edgerouter's MCP-certificate
* `client-cert-key` Path to the MCP-certificate private key
* `cert-path` Path to the edgerouter's TLS-certificate. **Does not have to be an MCP-certificate. In many cases it will
  be from a trusted TLS-ca, such as Let's Encrypt**
* `cert-key-path` Path to the TLS-certificate private key
* `client-ca` Path to a file containing a list of client CAs that can connect to this Edge Router. This is necessary for
  proper validation of client (Agent) certificates
* `tlsca` Path to a file containing trusted TLS CA certificates to use when connecting to the Router. If not set, the
  system CA store is used.
* `l` Location of the actual instance in ISO 3166 country code format. This is to be used for monitoring.
* `i` Allow insecure TLS, i.e. no validation of CA.
* `d` Debug statements are printed
* `prometheus-port` The port number for the Prometheus metrics server. Defaults to `2112`.
* `health-port` The port number for the plaintext health check server, exposing a `/health` endpoint. Defaults to
  `8889`.
* `skip-revocation-check` Allow client certificates that do not have OCSP or CRL endpoints for revocation checking.

#### VDE transport

The Edge Router can carry MMS traffic over VDES in addition to the IP/websocket link. The same binary serves both
roles; only the attachment differs.

* `vde-enabled` Enable the VDE transport. Defaults to `false`.
* `vde-mode` Where this Edge Router sits: `shore` (or `core`) connects to the VDE core network over TCP and lets it
  perform VDE framing, `ship` (or `modem`) drives the ship modem over the PI interface. Defaults to `shore`.
* `vde-core-addr` `host:port` of the VDE core network service, used in `shore` mode. Defaults to `127.0.0.1:9000`.
* `vde-modem-addr` `host:port` of the ship modem PI service, used in `ship` mode. Defaults to `127.0.0.1:60030`.
* `vde-preferred` Transport preference for outgoing traffic: `ip`, `vde` or `auto`. Defaults to `auto`, which uses the
  VDE link for small messages and the IP link for payloads above 8192 bytes, mirroring the VDE-TER payload limit.
* `vde-mrn-mmsi` Optional static `MRN=MMSI[,MRN=MMSI...]` pairs used to preload the MRN↔MMSI mapping.
* `vde-map-ttl` Time-to-live for MRN↔MMSI bindings learned from uplink traffic. Defaults to `15m`.

Ship mode example:

`./edgerouter -raddr "wss://127.0.0.1:8080" -port 7000 -mrn urn:mrn:mcp:device:mcc-test:ship-er -vde-enabled -vde-mode ship -vde-modem-addr 192.168.1.10:60030 -vde-preferred auto -vde-mrn-mmsi "urn:mrn:mcp:device:mcc-test:shore-er=992359599"`

Shore mode example:

`./edgerouter -raddr "wss://127.0.0.1:8080" -port 7000 -mrn urn:mrn:mcp:device:mcc-test:shore-er -vde-enabled -vde-mode shore -vde-core-addr 10.0.0.5:9000 -vde-preferred auto`

#### Usage example

`./edgerouter -raddr "wss://127.0.0.1:8080" -port 7000 -client-cert cc.pem -client-cert-key ccpk.pem -cert-path tls.crt -cert-key-path tlspk.key -client-ca ca-chain.pem -mrn urn:mrn:mcp:device:mcc-test:testedgerouter -l "SWE"`

#### Note:

It is perfectly fine to start the edgerouter, without the router at `-raddr` running. The edgerouter will continuously
probe the router, and establish a connection once possible.

### Router

The followings flags can be provided to the MMS router. Note that not specifying the proper certificates imposes
restrictions on the allowed operations.

#### Flags

* `port` The port number that this router should listen on. Edgerouters shall use this port to connect to the router. '
* `libp2p-port` The libp2p port exposed by this router to the MMS router network for UDP/QUIC
* `libp2p-tcp-port` The TCP port for libp2p. Defaults to the same as `libp2p-port` if not set.
* `privkey` Path to a file containing a private key for use within libp2p. If none is provided, a new private key will
  be generated every time the program is run. To uniquely identify a router and connect to that router (through the
  `beacons.txt`) configuration, a key must be provided.
* `cert-path` Path to the router's TLS-certificate. **Does not have to be an MCP-certificate. In many cases it will be
  from a trusted TLS-ca, such as Let's Encrypt**
* `cert-key-path` Path to the TLS-certificate private key
* `client-ca` Path to a file containing a list of client CAs that can connect to this router. This is necessary for
  proper validation of client (edgerouter) certificates
* `d` Debug statements are printed
* `beacons` Path to a file containing known routers that this router can use to connect to the libp2p network. If not
  set the router will search for a `beacons.txt` file in its own directory.
* `l` Location of the actual instance in ISO 3166 country code format. This is to be used for monitoring.
* `prometheus-port` The port number for the Prometheus metrics server. Defaults to `2113`.
* `health-port` The port number for the plaintext health check server, exposing a `/health` endpoint. Defaults to
  `8081`.
* `skip-revocation-check` Allow client certificates that do not have OCSP or CRL endpoints for revocation checking.

#### Usage example

`./router -libp2p-port 27000 -privkey pk.key -cert-path tls.crt -cert-key-path tlspk.key -client-ca ca-chain.pem -l "SWE"`

<a id="agent"></a>

## Setting up an MMS Agent

An MMS agent is required for sending and receiving messages to/from the MMS network. An MMS agent interfaces with the
MMS network by establishing a connection with an edgerouter and sending
MMTP protocol messages.  
It is up to the user of the MMS to implement their own agent, but here we provide a few key points to keep in mind when
implementing an agent:

* An agent shall establish a [websockets](https://developer.mozilla.org/en-US/docs/Web/API/WebSockets_API) connection
  with an ederouter on its specified `-port` before sending any MMTP-messages
* An agent must be authenticated to send MMTP-messages and to receive MRN-adressed messages. To connect authenticated to
  the edgerouter, the agent must present a valid MCP-certificate when connecting over websockets.  
  In many websocket libraries, this can be done by creating an **ssl context**, where a CA-file, a certificate file and
  a private key file is provided.
* An agent can connect unauthenticated, but may in that case only receive subject-cast MMTP-messages, when subscribed to
  that particular subject.
