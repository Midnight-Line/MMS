#!/usr/bin/env python3
"""Fake VDE core network for the MMS deploy test bed.

Two kinds of peers connect to this process:

  core   the VDE-TER gateway Edge Router. It speaks the JSON line protocol of
         the VDE core network interface: one JSON object per line, types
         ``downlink`` (business system to core network), ``uplink`` (core
         network to business system) and ``ack`` (delivery receipt).

  radio  the fake ship modems. They register the MMSI they serve and then
         exchange ``downlink``/``uplink``/``delivered`` lines with this
         process, standing in for the VDE air interface between the ship
         station and the VDE-TER gateway.

Routing rules:

  * a ``downlink`` from a core peer is acknowledged with ``received``, then
    handed to the radio peer serving the target MMSI (MMSI 0 broadcasts to
    every radio peer), acknowledged with ``sent`` and finally with
    ``delivered`` once the radio peer confirms it passed the payload on;
  * an ``uplink`` from a radio peer is forwarded to every connected core peer.

Every peer connection is newline delimited JSON, so the whole test bed can be
inspected with nothing but netcat.
"""

import argparse
import base64
import json
import socket
import threading
import time

# MMSI used as the origin of traffic that enters the VDE system from the
# gateway Edge Router, i.e. the VDE-TER identity of the gateway itself.
DEFAULT_CORE_MMSI = 992359000


class Hub:
    """Registry of connected peers and the routing between them."""

    def __init__(self, core_mmsi, verbose):
        self.core_mmsi = core_mmsi
        self.verbose = verbose
        self.lock = threading.Lock()
        self.core_peers = []
        self.radio_peers = {}  # mmsi -> Peer
        self.stats = {"downlink": 0, "uplink": 0, "delivered": 0, "failed": 0}

    def log(self, message):
        if self.verbose:
            print(f"[corenet] {message}", flush=True)

    # -- peer bookkeeping -------------------------------------------------
    def add_core(self, peer):
        with self.lock:
            self.core_peers.append(peer)
        self.log(f"gateway connected from {peer.addr}")

    def remove_core(self, peer):
        with self.lock:
            if peer in self.core_peers:
                self.core_peers.remove(peer)
        self.log(f"gateway disconnected ({peer.addr})")

    def add_radio(self, peer, mmsi):
        with self.lock:
            previous = self.radio_peers.get(mmsi)
            self.radio_peers[mmsi] = peer
        if previous is not None and previous is not peer:
            self.log(f"MMSI {mmsi} re-registered, dropping the previous peer")
            previous.close()
        self.log(f"ship modem registered for MMSI {mmsi} from {peer.addr}")

    def remove_radio(self, peer):
        with self.lock:
            for mmsi, registered in list(self.radio_peers.items()):
                if registered is peer:
                    del self.radio_peers[mmsi]
                    self.log(f"ship modem for MMSI {mmsi} disconnected")
                    break

    # -- message flow -----------------------------------------------------
    def handle_downlink(self, peer, message):
        """Business system wants to send something over the air interface."""
        identifier = message.get("id", "")
        mmsi = int(message.get("mmsi", 0) or 0)
        payload = message.get("data", "")

        self.stats["downlink"] += 1
        self.log(f"downlink {identifier} to MMSI {mmsi} ({len(payload)} base64 chars)")
        peer.send({"type": "ack", "id": identifier, "status": "received"})

        with self.lock:
            if mmsi == 0:
                targets = list(self.radio_peers.values())
            else:
                target = self.radio_peers.get(mmsi)
                targets = [target] if target is not None else []

        if not targets:
            self.stats["failed"] += 1
            self.log(f"no ship modem is serving MMSI {mmsi}")
            peer.send(
                {
                    "type": "ack",
                    "id": identifier,
                    "status": "failed",
                    "reason": f"no ship modem is serving MMSI {mmsi}",
                }
            )
            return

        for target in targets:
            target.send(
                {
                    "type": "downlink",
                    "id": identifier,
                    "mmsi": mmsi,
                    "src": self.core_mmsi,
                    "data": payload,
                }
            )
        peer.send({"type": "ack", "id": identifier, "status": "sent"})

    def handle_uplink(self, peer, message):
        """A ship modem handed a payload to the air interface."""
        mmsi = int(message.get("mmsi", 0) or 0)
        payload = message.get("data", "")

        self.stats["uplink"] += 1
        self.log(f"uplink from MMSI {mmsi} ({len(payload)} base64 chars)")

        with self.lock:
            cores = list(self.core_peers)

        if not cores:
            self.log("dropping uplink, no gateway is connected")
            return

        for core in cores:
            core.send({"type": "uplink", "mmsi": mmsi, "data": payload})

    def handle_delivered(self, peer, message):
        identifier = message.get("id", "")
        self.stats["delivered"] += 1
        self.log(f"delivered {identifier}")
        with self.lock:
            cores = list(self.core_peers)
        for core in cores:
            core.send({"type": "ack", "id": identifier, "status": "delivered"})

    def status(self):
        with self.lock:
            radios = sorted(self.radio_peers)
            cores = len(self.core_peers)
        return {"gateways": cores, "ship_modems": radios, "stats": dict(self.stats)}


class Peer(threading.Thread):
    """One connected peer, either a gateway or a ship modem."""

    def __init__(self, hub, conn, addr):
        super().__init__(daemon=True)
        self.hub = hub
        self.conn = conn
        self.addr = addr
        self.write_lock = threading.Lock()
        self.role = None
        self.mmsi = None

    def send(self, message):
        data = (json.dumps(message) + "\n").encode("utf-8")
        with self.write_lock:
            try:
                self.conn.sendall(data)
            except OSError:
                pass

    def close(self):
        try:
            self.conn.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
        try:
            self.conn.close()
        except OSError:
            pass

    def run(self):
        buffer = b""
        try:
            while True:
                chunk = self.conn.recv(65536)
                if not chunk:
                    break
                buffer += chunk
                while b"\n" in buffer:
                    line, buffer = buffer.split(b"\n", 1)
                    line = line.strip()
                    if line:
                        self.dispatch(line)
        except OSError:
            pass
        finally:
            if self.role == "core":
                self.hub.remove_core(self)
            elif self.role == "radio":
                self.hub.remove_radio(self)
            self.close()

    def dispatch(self, line):
        try:
            message = json.loads(line.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as err:
            self.hub.log(f"ignoring malformed line from {self.addr}: {err}")
            return

        kind = message.get("type", "")

        if self.role is None:
            # The first message tells the roles apart: ship modems register,
            # the gateway starts straight away with a downlink or a ping.
            if kind == "register":
                self.role = "radio"
                self.mmsi = int(message.get("mmsi", 0) or 0)
                self.hub.add_radio(self, self.mmsi)
                self.send({"type": "registered", "mmsi": self.mmsi})
                return
            self.role = "core"
            self.hub.add_core(self)

        if kind == "downlink":
            self.hub.handle_downlink(self, message)
        elif kind == "uplink":
            self.hub.handle_uplink(self, message)
        elif kind == "delivered":
            self.hub.handle_delivered(self, message)
        elif kind == "ping":
            self.send({"type": "pong"})
        elif kind == "status":
            self.send({"type": "status", "result": self.hub.status()})
        else:
            self.hub.log(f"ignoring unknown message type {kind!r} from {self.addr}")


def serve(port, hub):
    server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    server.bind(("127.0.0.1", port))
    server.listen(16)
    print(f"[corenet] listening on 127.0.0.1:{port}", flush=True)
    try:
        while True:
            conn, addr = server.accept()
            Peer(hub, conn, f"{addr[0]}:{addr[1]}").start()
    except KeyboardInterrupt:
        print("[corenet] stopping", flush=True)
    finally:
        server.close()


def main():
    parser = argparse.ArgumentParser(description="Fake VDE core network")
    parser.add_argument("--port", type=int, default=9000, help="TCP port to listen on")
    parser.add_argument(
        "--core-mmsi",
        type=int,
        default=DEFAULT_CORE_MMSI,
        help="MMSI reported as the origin of gateway originated traffic",
    )
    parser.add_argument("-v", "--verbose", action="store_true", help="log every message")
    args = parser.parse_args()

    hub = Hub(args.core_mmsi, args.verbose)
    serve(args.port, hub)


if __name__ == "__main__":
    main()
