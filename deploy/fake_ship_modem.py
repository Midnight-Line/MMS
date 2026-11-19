#!/usr/bin/env python3
"""Fake VDE ship modem for the MMS deploy test bed.

The modem sits between one ship Edge Router and the fake VDE core network and
plays both halves of a real ship station:

  * towards the Edge Router it is the presentation interface (PI): it speaks
    IEC 61162 style ``$VDTDB``/``$VDEDM``/``$VDESI`` sentences with 6 bit
    ASCII payloads over TCP;
  * towards the fake core network it is a radio peer: it registers the MMSI it
    serves and exchanges base64 payloads, standing in for the VDE air
    interface that the VDE-TER gateway terminates.

Traffic flow:

  ship Edge Router --TDB--> modem --uplink--> core network --uplink--> gateway
  gateway --downlink--> core network --downlink--> modem --EDM/ESI--> ship Edge Router

The 6 bit alphabet and the sentence framing match ``vde/pi`` exactly, so the
Go code parses these sentences without any special casing.
"""

import argparse
import base64
import json
import socket
import threading
import time

# ITU-R M.1371 six bit ASCII, as used by vde/pi.Encode6Bit.
SIXBIT = (
    "@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\\]^_"
    " !\"#$%&'()*+,-./"
    "0123456789:;<=>?"
)
DECODE_SIXBIT = {char: index for index, char in enumerate(SIXBIT)}

# Message numbers from the VDES PI sentence design.
MSG_START = 74
MSG_CONTINUE = 75
MSG_END = 76

# Maximum number of 6 bit characters in the data field of one TDB sentence.
MAX_FRAGMENT_CHARS = 1400


def encode_sixbit(payload):
    """Encode bytes into the 6 bit ASCII alphabet, MSB first, zero padded."""
    accumulator = 0
    bits = 0
    out = []
    for byte in payload:
        accumulator = (accumulator << 8) | byte
        bits += 8
        while bits >= 6:
            bits -= 6
            out.append(SIXBIT[(accumulator >> bits) & 0x3F])
    if bits:
        out.append(SIXBIT[(accumulator << (6 - bits)) & 0x3F])
    return "".join(out)


def decode_sixbit(text):
    """Decode a 6 bit ASCII string back into bytes, dropping the padding."""
    accumulator = 0
    bits = 0
    out = bytearray()
    for char in text:
        value = DECODE_SIXBIT.get(char)
        if value is None:
            raise ValueError(f"invalid 6 bit character {char!r}")
        accumulator = (accumulator << 6) | value
        bits += 6
        if bits >= 8:
            bits -= 8
            out.append((accumulator >> bits) & 0xFF)
    return bytes(out)


def checksum(payload):
    value = 0
    for char in payload:
        value ^= ord(char)
    return value & 0xFF


def sentence(talker, formatter, fields):
    """Build one NMEA style sentence with the payload checksum."""
    payload = talker + formatter
    if fields:
        payload += "," + ",".join(str(field) for field in fields)
    return f"${payload}*{checksum(payload):02X}\r\n"


def parse_sentence(line):
    """Split a sentence into talker, formatter and fields, verifying the checksum."""
    line = line.strip("\r\n")
    if len(line) < 2 or line[0] not in "$!":
        raise ValueError("not a sentence")
    star = line.rfind("*")
    if star < 0 or star + 3 > len(line):
        raise ValueError("missing checksum")
    payload = line[1:star]
    expected = int(line[star + 1 :], 16)
    actual = checksum(payload)
    if actual != expected:
        raise ValueError(f"checksum mismatch, got {actual:02X} want {expected:02X}")
    parts = payload.split(",")
    head = parts[0]
    if len(head) < 5:
        raise ValueError(f"header {head!r} too short")
    return head[:2], head[2:], parts[1:]


def split_data(fields, data_index):
    """Split off the data field, which may itself contain commas."""
    if len(fields) < data_index + 2:
        raise ValueError("too few fields")
    head = fields[:data_index]
    tail = fields[len(fields) - 1]
    data = ",".join(fields[data_index : len(fields) - 1])
    return head, data, tail


def as_int(value, default=0):
    value = (value or "").strip()
    if not value:
        return default
    return int(value)


class Assembler:
    """Reassembles a fragmented payload from TDB fragments."""

    def __init__(self):
        self.pending = None

    def push(self, msg_number, data):
        if msg_number == MSG_START:
            self.pending = bytearray(data)
            return None
        if self.pending is None:
            return None
        if msg_number == MSG_CONTINUE:
            self.pending.extend(data)
            return None
        if msg_number == MSG_END:
            self.pending.extend(data)
            complete = bytes(self.pending)
            self.pending = None
            return complete
        return None


class Modem:
    """The ship modem: PI server for the Edge Router, radio peer for the core network."""

    def __init__(self, args):
        self.args = args
        self.mmsi = args.mmsi
        self.verbose = args.verbose
        self.pi_conn = None
        self.pi_lock = threading.Lock()
        self.core_conn = None
        self.core_lock = threading.Lock()
        self.assembler = Assembler()
        self.sequence = 0
        self.stats = {"from_ship": 0, "to_ship": 0}

    def log(self, message):
        if self.verbose:
            print(f"[modem {self.mmsi}] {message}", flush=True)

    # -- core network side ------------------------------------------------
    def connect_core(self):
        while True:
            try:
                conn = socket.create_connection(
                    (self.args.core_host, self.args.core_port), timeout=5
                )
            except OSError as err:
                self.log(f"core network unreachable: {err}, retrying")
                time.sleep(2)
                continue
            conn.settimeout(None)
            with self.core_lock:
                self.core_conn = conn
            self.log("connected to the fake core network")
            self.send_core({"type": "register", "mmsi": self.mmsi})
            self.pump_core(conn)
            with self.core_lock:
                self.core_conn = None
            try:
                conn.close()
            except OSError:
                pass
            self.log("core network connection lost, reconnecting")
            time.sleep(2)

    def send_core(self, message):
        with self.core_lock:
            conn = self.core_conn
            if conn is None:
                return
            try:
                conn.sendall((json.dumps(message) + "\n").encode("utf-8"))
            except OSError:
                pass

    def pump_core(self, conn):
        buffer = b""
        while True:
            try:
                chunk = conn.recv(65536)
            except OSError:
                return
            if not chunk:
                return
            buffer += chunk
            while b"\n" in buffer:
                line, buffer = buffer.split(b"\n", 1)
                line = line.strip()
                if not line:
                    continue
                try:
                    message = json.loads(line.decode("utf-8"))
                except (UnicodeDecodeError, json.JSONDecodeError) as err:
                    self.log(f"ignoring malformed line: {err}")
                    continue
                self.handle_core(message)

    def handle_core(self, message):
        kind = message.get("type", "")
        if kind == "downlink":
            self.deliver_to_ship(message)
        elif kind in ("registered", "pong", "status", "ack"):
            self.log(f"core network said {json.dumps(message)}")
        else:
            self.log(f"ignoring core network message {kind!r}")

    def deliver_to_ship(self, message):
        """Push a payload to the ship Edge Router as EDM plus ESI."""
        identifier = message.get("id", "")
        try:
            payload = base64.b64decode(message.get("data", ""))
        except (ValueError, TypeError) as err:
            self.log(f"dropping downlink {identifier}: bad base64 ({err})")
            return

        source_mmsi = int(message.get("src", 0) or 0)
        encoded = encode_sixbit(payload)

        # EDM carries the payload; the transmitting station MMSI follows in ESI.
        edm = sentence("VD", "EDM", [self.next_sequence(), "", "", encoded, 0])
        esi = sentence("VD", "ESI", [self.next_sequence(), source_mmsi])
        self.write_pi(edm + esi)

        self.stats["to_ship"] += 1
        self.log(f"delivered downlink {identifier} to the ship ({len(payload)} bytes)")
        self.send_core({"type": "delivered", "id": identifier})

    # -- presentation interface side --------------------------------------
    def next_sequence(self):
        self.sequence = (self.sequence + 1) % 100
        return self.sequence

    def write_pi(self, data):
        with self.pi_lock:
            conn = self.pi_conn
            if conn is None:
                return
            try:
                conn.sendall(data.encode("ascii"))
            except OSError:
                pass

    def serve_pi(self):
        server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        server.bind(("127.0.0.1", self.args.port))
        server.listen(4)
        print(
            f"[modem {self.mmsi}] PI listening on 127.0.0.1:{self.args.port}",
            flush=True,
        )
        while True:
            conn, addr = server.accept()
            self.log(f"ship Edge Router connected from {addr[0]}:{addr[1]}")
            with self.pi_lock:
                self.pi_conn = conn
            threading.Thread(target=self.pump_pi, args=(conn,), daemon=True).start()

    def pump_pi(self, conn):
        buffer = b""
        while True:
            try:
                chunk = conn.recv(65536)
            except OSError:
                break
            if not chunk:
                break
            buffer += chunk
            while b"\n" in buffer:
                line, buffer = buffer.split(b"\n", 1)
                text = line.decode("ascii", "replace").strip()
                if text:
                    self.handle_pi(text)
        with self.pi_lock:
            if self.pi_conn is conn:
                self.pi_conn = None
        self.log("ship Edge Router disconnected")

    def handle_pi(self, line):
        try:
            talker, formatter, fields = parse_sentence(line)
        except ValueError as err:
            self.log(f"ignoring malformed sentence: {err}")
            return

        if formatter == "TDB":
            self.handle_tdb(fields)
        elif formatter == "TSB":
            self.handle_tsb(fields)
        elif formatter in ("TMK", "TSK", "EDO", "ESI", "EDM"):
            self.log(f"ignoring {formatter} from the ship")
        else:
            self.log(f"ignoring {talker}{formatter} from the ship")

    def handle_tdb(self, fields):
        try:
            head, data, tail = split_data(fields, 7)
            total = as_int(head[0])
            number = as_int(head[1])
            message_seq = as_int(head[2])
            source_id = as_int(head[3])
            dest_id = as_int(head[4])
            priority = as_int(head[5])
            data_type = as_int(head[6])
            fill_bits = as_int(tail)
            payload = decode_sixbit(data)
        except (ValueError, IndexError) as err:
            self.log(f"ignoring malformed TDB: {err}")
            return

        self.log(
            f"TDB {number}/{total} seq {message_seq} dest {dest_id} "
            f"type {data_type} priority {priority} ({len(payload)} bytes)"
        )

        if data_type in (MSG_START, MSG_CONTINUE, MSG_END):
            # A message that fits in one sentence is sent as a lone Start
            # fragment, so there is no End fragment to complete the chain.
            if total <= 1:
                pass
            else:
                complete = self.assembler.push(data_type, payload)
                if complete is None:
                    return
                payload = complete
        else:
            self.log(f"unexpected TDB data type {data_type}, treating it as self contained")

        self.stats["from_ship"] += 1
        self.send_core(
            {
                "type": "uplink",
                "mmsi": self.mmsi,
                "data": base64.b64encode(payload).decode("ascii"),
            }
        )
        self.log(f"handed {len(payload)} bytes to the core network")

    def handle_tsb(self, fields):
        try:
            head, data, tail = split_data(fields, 7)
            dest_id = as_int(head[4])
            payload = decode_sixbit(data)
        except (ValueError, IndexError) as err:
            self.log(f"ignoring malformed TSB: {err}")
            return

        self.log(f"TSB dest {dest_id} ({len(payload)} bytes)")
        self.stats["from_ship"] += 1
        self.send_core(
            {
                "type": "uplink",
                "mmsi": self.mmsi,
                "data": base64.b64encode(payload).decode("ascii"),
            }
        )

    def run(self):
        threading.Thread(target=self.connect_core, daemon=True).start()
        self.serve_pi()


def main():
    parser = argparse.ArgumentParser(description="Fake VDE ship modem")
    parser.add_argument("--port", type=int, required=True, help="PI TCP port for the Edge Router")
    parser.add_argument(
        "--mmsi", type=int, required=True, help="MMSI this ship station is registered as"
    )
    parser.add_argument("--core-host", default="127.0.0.1", help="Fake core network host")
    parser.add_argument("--core-port", type=int, default=9000, help="Fake core network port")
    parser.add_argument("-v", "--verbose", action="store_true", help="log every sentence")
    args = parser.parse_args()

    Modem(args).run()


if __name__ == "__main__":
    main()
