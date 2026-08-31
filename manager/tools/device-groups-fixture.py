#!/usr/bin/env python3
"""Run two isolated, real Rust OPC servers against Linux PTY device simulators.

Python 3.8+ standard library only. No build is performed by this script.

Example (run on the remote Linux test machine only)::

    python3 manager/tools/device-groups-fixture.py \
      --binary /path/to/compiled/rcd_server \
      --root /path/to/new/isolated-run --port-a 4845 --port-b 4846

The root must be new or empty. ``Fixture.json`` records the fixture/server PIDs,
endpoints, source-local port IDs, expected values and all log paths. Wait until
its state is ``ready`` before running OPC tests; readiness only proves TCP
listeners and actual serial polling, not the OPC test assertions themselves.

Change ``control.json`` atomically while running; missing fields keep defaults::

    {
      "a": {"server_mode": "running", "ack_mode": "success", "delay_ms": 20,
            "values": {"1": 11, "2": 22}, "poll_mode": "respond"},
      "b": {"ack_mode": "drop", "delay_ms": 20,
            "values": {"1": 101, "2": 202}, "poll_mode": "respond"}
    }

ack_mode is success/drop/bad; bad emits 00 00 and must NOT be accepted as an ACK.
The actual RCD ACK is 00 50 50 (src/serial/manager.rs COMMAND_ACK_BYTES).
poll_mode=drop simulates device silence without closing the OPC server.
server_mode=stopped gracefully stops only that Rust instance while keeping its
PTY/configuration/PKI. Set running to restart it in the same directory. The
other source continues being simulated throughout shutdown/startup. Inspect
sources.a/b.process_state and pid in Fixture.json for the completed transition.
delay_ms is captured when a command is received, as are that command's mode
and source. Later control changes do not retarget or alter a pending response.

The wire command is logged including the checksum appended by Rust; body_hex
is the original OPC payload. Use non-query commands such as 01 20 AA / 02 20 BB
to distinguish commands from automatic READ_STATUS polls. Arbitrary command
frames have no universal length field, so non-poll frames are separated by a
10 ms idle gap (the configured Rust poll interval is 20 ms).

The script only owns its newly created PTYs and subprocesses. SIGINT/SIGTERM
stops those children, closes those PTYs and removes their own alias symlinks.
Configuration, PKI, stdout, poll summaries and command logs remain for review.
"""

import argparse
import collections
import csv
import datetime
import errno
import heapq
import json
import os
from pathlib import Path
import pty
import selectors
import shutil
import signal
import socket
import subprocess
import sys
import termios
import time
import tty


POLL_PREFIX = b"\x08\x02\x30\x10\x01"
ACK_SUCCESS = b"\x00\x50\x50"
COMMAND_IDLE_SECONDS = 0.010
SERIAL_TIMEOUT_MS = 50
SUMMARY_INTERVAL_SECONDS = 10.0
CONTROL_INTERVAL_SECONDS = 0.10
MAX_COMMAND_BYTES = 65536
DEFAULT_VALUES = {"a": {1: 11, 2: 22}, "b": {1: 101, 2: 202}}
DEFAULT_ACK_DELAY_MS = 20


def timestamp():
    return datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="milliseconds")


def hex_text(value):
    return " ".join("%02X" % byte for byte in value)


def write_json_atomic(path, value):
    temporary = path.with_name(".%s.%d.tmp" % (path.name, os.getpid()))
    with temporary.open("w", encoding="utf-8") as output:
        json.dump(value, output, ensure_ascii=False, indent=2)
        output.write("\n")
        output.flush()
        os.fsync(output.fileno())
    os.replace(str(temporary), str(path))


def emit_json(output, value):
    output.write(json.dumps(value, ensure_ascii=False, separators=(",", ":")) + "\n")
    output.flush()


def checksum_frame(body):
    return body + bytes([sum(body) & 0xFF])


def value_map(source_values):
    return {
        "A_VALUE": source_values[1],
        "B_VALUE": source_values[2],
        "CALC_A": source_values[1] * 2,
        "CALC_AB": source_values[1] + source_values[2],
        "CALC_CONST": 7,
    }


def config_text(primary_port, secondary_port):
    # Both run_on/current_run MUST be primary so the Rust worker actively polls.
    # Current main.rs only starts OPC; independent TCP addresses also avoid
    # collisions if the optional TCP service is re-enabled later.
    return """[server]
primary_ip = "127.0.0.1:%d"
secondary_ip = "127.0.0.1:%d"
run_on = "primary"
current_run = "primary"

[serial]
baud_rate = 115200
data_bits = "Eight"
parity = "None"
stop_bits = "One"
flow_control = "none"

[settings]
timeout = %d
poll_interval_ms = 1
command_retry_count = 1
query_retry_count = 0
parity_switch_delay_ms = 1
transmit_settle_guard_ms = 1

[logging]
level = "warn,rcd_server=info"
directory = "logs"
basename = "rcd_server_fixture"
duplicate_to_stderr = "warn"
rotate_age = "none"
rotate_size_bytes = 52428800
keep_files = 5
write_mode = "direct"
serial_frame_logs = false
opcua_transport_errors = false
""" % (primary_port, secondary_port, SERIAL_TIMEOUT_MS)


def write_device_csv(path, port_id, address):
    # Keep every row at nine columns: the real CSV reader is not flexible.
    headers = [
        ("com", port_id),
        ("baud_rate", "115200"),
        # The legacy name is_special means transmit whole-frame without
        # Mark/Space parity changes. PTYs cannot reproduce physical 9-bit I/O.
        ("is_special", "true"),
        ("com_index", "1"),
        ("device_id", str(address)),
        ("data_len", "1"),
        ("kks_prefix", ""),
    ]
    point = "A_VALUE" if address == 1 else "B_VALUE"
    with path.open("w", encoding="utf-8", newline="") as output:
        writer = csv.writer(output, lineterminator="\n")
        for name, value in headers:
            writer.writerow([name, value] + [""] * 7)
        writer.writerow(["kks", "type_", "f_type", "byte_index", "bit_index", "def", "max", "min", "lh"])
        writer.writerow([point, "uint", "status", "1", "0.7", "0", "255", "0", "0"])


def write_calc_config(path):
    with path.open("w", encoding="utf-8") as output:
        for node, expression in [
            ("CALC_A", "A_VALUE * 2"),
            ("CALC_AB", "A_VALUE + B_VALUE"),
            ("CALC_CONST", "7"),
        ]:
            output.write("[[rules]]\nkks_calc = %s\ndata_type = \"uint\"\nexpression = %s\n\n" % (
                json.dumps(node), json.dumps(expression)))


def reserve_port(port=0):
    reservation = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        reservation.bind(("0.0.0.0", port))
        return reservation
    except Exception:
        reservation.close()
        raise


class Source:
    def __init__(self, name, endpoint_port, root):
        self.name = name
        self.endpoint_port = endpoint_port
        self.run_dir = root / name
        self.run_dir.mkdir()
        self.config_dir = self.run_dir / "config"
        self.config_dir.mkdir()
        self.log_dir = self.run_dir / "logs"
        self.log_dir.mkdir()
        self.alias = self.run_dir / ("serial_%s" % name)
        self.master_fd = None
        self.slave_fd = None
        self.slave_name = None
        self.child = None
        self.console = None
        self.buffer = bytearray()
        self.last_rx = 0.0
        self.polls = collections.Counter()
        self.responses = collections.Counter()
        self.last_poll_frames = {}
        self.last_response_frames = {}
        self.commands = 0
        self.invalid_frames = 0
        self.acks_sent = 0
        self.acks_dropped = 0
        self.io_errors = 0
        self.expected_values = dict(DEFAULT_VALUES[name])
        self.control = self.default_control()
        self.tcp_ready = False
        self.process_state = "prepared"
        self.stop_requested_at = None
        self.stop_escalation = 0
        self.generation = 0
        self.started_monotonic = 0.0
        self.poll_baseline = {}
        self.primary_port = None
        self.secondary_port = None

    def default_control(self):
        return {"server_mode": "running", "ack_mode": "success", "delay_ms": DEFAULT_ACK_DELAY_MS,
                "poll_mode": "respond", "values": dict(DEFAULT_VALUES[self.name])}

    def prepare(self, binary, primary_port, secondary_port):
        self.primary_port = primary_port
        self.secondary_port = secondary_port
        target = self.run_dir / "rcd_server"
        shutil.copy2(str(binary), str(target))
        if not os.access(str(target), os.X_OK):
            target.chmod(target.stat().st_mode | 0o100)
        self.master_fd, self.slave_fd = pty.openpty()
        tty.setraw(self.master_fd)
        tty.setraw(self.slave_fd)
        os.set_blocking(self.master_fd, False)
        self.slave_name = os.ttyname(self.slave_fd)
        # Alias is created only under the new run root, never under /dev.
        os.symlink(self.slave_name, str(self.alias))
        with (self.config_dir / "config.toml").open("w", encoding="utf-8") as output:
            output.write(config_text(primary_port, secondary_port))
        for address in (1, 2):
            write_device_csv(self.config_dir / ("rcd_device_%d.csv" % address), str(self.alias), address)
        write_calc_config(self.config_dir / "calc.toml")

    def start(self):
        if self.console:
            self.console.close()
        # A new Rust generation must not consume an ACK buffered for a stopped
        # process. Flush only the fixture-owned PTY, never a physical port.
        termios.tcflush(self.slave_fd, termios.TCIOFLUSH)
        self.buffer.clear()
        environment = os.environ.copy()
        environment["RCD_OPCUA_PORT"] = str(self.endpoint_port)
        # Remove only known Rust log overrides that would defeat per-run logs.
        for name in ("RCD_CONFIG", "RUST_LOG", "RCD_LOG_SERIAL_FRAMES", "RCD_LOG_OPCUA_TRANSPORT_ERRORS"):
            environment.pop(name, None)
        self.console = (self.log_dir / "console.log").open("ab", buffering=0)
        self.child = subprocess.Popen(
            [str(self.run_dir / "rcd_server")], cwd=str(self.run_dir),
            env=environment, stdin=subprocess.DEVNULL, stdout=self.console,
            stderr=subprocess.STDOUT, close_fds=True, start_new_session=True,
        )
        self.process_state = "starting"
        self.stop_requested_at = None
        self.stop_escalation = 0
        self.generation += 1
        self.started_monotonic = time.monotonic()
        self.poll_baseline = dict(self.polls)
        self.tcp_ready = False

    def metadata(self):
        return {
            "pid": self.child.pid if self.child and self.child.poll() is None else None,
            "last_pid": self.child.pid if self.child else None,
            "exit_code": self.child.poll() if self.child else None,
            "process_state": self.process_state,
            "requested_server_mode": self.control["server_mode"],
            "generation": self.generation,
            "endpoint": "opc.tcp://127.0.0.1:%d/" % self.endpoint_port,
            "opc_port": self.endpoint_port,
            "port_id": str(self.alias),
            "port_name": self.alias.name,
            "pty_slave": self.slave_name,
            "send_node_id": self.alias.name + "_Send",
            "send_method_id": self.alias.name + "_SendCommand",
            "method_object_id": "Methods",
            "business_namespace_uri": "urn:SerialServer",
            "topology_namespace_uri": "urn:SerialServer:topology",
            "manifest_node_id": "Topology/Manifest",
            "primary_address": "127.0.0.1:%d" % self.primary_port,
            "secondary_address": "127.0.0.1:%d" % self.secondary_port,
            "role": "primary",
            "run_dir": str(self.run_dir),
            "config_dir": str(self.config_dir),
            "pki_dir": str(self.run_dir / "pki"),
            "log_dir": str(self.log_dir),
            "console_log": str(self.log_dir / "console.log"),
            "devices": [
                {"device_id": str(address), "device_address": address,
                 "node_ids": ["A_VALUE" if address == 1 else "B_VALUE"]}
                for address in (1, 2)
            ],
            "expected_values": value_map(self.expected_values),
            "current_values": value_map(self.control["values"]),
            "tcp_ready": self.tcp_ready,
            "poll_counts": dict(self.polls),
        }

    def stop(self):
        if self.child and self.child.poll() is None:
            # SIGINT is handled by Rust's graceful cancellation path.
            try:
                self.child.send_signal(signal.SIGINT)
            except ProcessLookupError:
                pass

    def finish_stop(self):
        if self.child and self.child.poll() is None:
            try:
                self.child.wait(timeout=4.0)
            except subprocess.TimeoutExpired:
                self.child.terminate()
                try:
                    self.child.wait(timeout=2.0)
                except subprocess.TimeoutExpired:
                    self.child.kill()
                    self.child.wait(timeout=2.0)
        for name in ("master_fd", "slave_fd"):
            descriptor = getattr(self, name)
            if descriptor is not None:
                os.close(descriptor)
                setattr(self, name, None)
        if self.alias.is_symlink() and os.readlink(str(self.alias)) == self.slave_name:
            self.alias.unlink()
        if self.console:
            self.console.close()
        self.process_state = "stopped"
        self.tcp_ready = False


class Fixture:
    def __init__(self, args):
        self.args = args
        self.root = args.root
        self.started_at = timestamp()
        self.sources = []
        self.selector = selectors.DefaultSelector()
        self.stopping = False
        self.exit_reason = "running"
        self.exit_code = 0
        self.state = "starting"
        self.sequence = 0
        self.scheduled_acks = []
        self.control_signature = None
        self.last_control_error = None
        self.last_control_check = 0.0
        self.last_summary = time.monotonic()
        self.last_ready_check = 0.0
        self.command_log = (self.root / "commands.jsonl").open("a", encoding="utf-8")
        self.poll_log = (self.root / "polls.jsonl").open("a", encoding="utf-8")
        self.event_log = (self.root / "events.jsonl").open("a", encoding="utf-8")

    def event(self, event, **fields):
        value = {"at": timestamp(), "event": event}
        value.update(fields)
        emit_json(self.event_log, value)

    def manifest(self):
        value = {
            "schema_version": 1, "pid": os.getpid(), "state": self.state,
            "started_at": self.started_at, "updated_at": timestamp(),
            "binary": str(self.args.binary), "root": str(self.root),
            "control_file": str(self.root / "control.json"),
            "command_log": str(self.root / "commands.jsonl"),
            "poll_log": str(self.root / "polls.jsonl"),
            "event_log": str(self.root / "events.jsonl"),
            "serial_timeout_ms": SERIAL_TIMEOUT_MS,
            "command_retry_count": 1,
            "ack_success_hex": hex_text(ACK_SUCCESS),
            "sample_command_bodies": {"device_1": "01 20 AA", "device_2": "02 20 BB"},
            "sources": {source.name: source.metadata() for source in self.sources},
        }
        if self.state in ("stopped", "failed"):
            value["finished_at"] = timestamp()
            value["exit_reason"] = self.exit_reason
            value["exit_code"] = self.exit_code
        write_json_atomic(self.root / "Fixture.json", value)

    def initialize(self):
        reservations = []
        try:
            # Refuse occupied OPC ports; do not stop or replace whoever owns them.
            reservations.extend([reserve_port(self.args.port_a), reserve_port(self.args.port_b)])
            tcp = [reserve_port() for _ in range(4)]
            reservations.extend(tcp)
            tcp_ports = [reservation.getsockname()[1] for reservation in tcp]
            for index, (name, port) in enumerate([("a", self.args.port_a), ("b", self.args.port_b)]):
                source = Source(name, port, self.root)
                self.sources.append(source)
                source.prepare(self.args.binary, tcp_ports[index * 2], tcp_ports[index * 2 + 1])
                self.selector.register(source.master_fd, selectors.EVENT_READ, source)
            control = {}
            for source in self.sources:
                settings = source.default_control()
                settings["values"] = {str(key): value for key, value in settings["values"].items()}
                control[source.name] = settings
            write_json_atomic(self.root / "control.json", control)
            self.load_control(force=True)
        finally:
            for reservation in reservations:
                reservation.close()
        for source in self.sources:
            source.start()
            self.event("source_started", source=source.name, pid=source.child.pid,
                       endpoint=source.metadata()["endpoint"], port_id=str(source.alias))
        self.manifest()
        print("Fixture starting: %s" % (self.root / "Fixture.json"), flush=True)

    def load_control(self, force=False):
        now = time.monotonic()
        if not force and now - self.last_control_check < CONTROL_INTERVAL_SECONDS:
            return
        self.last_control_check = now
        path = self.root / "control.json"
        try:
            stat = path.stat()
            signature = (stat.st_mtime_ns, stat.st_size)
            if not force and signature == self.control_signature:
                return
            self.control_signature = signature
            if stat.st_size > 65536:
                raise ValueError("control.json exceeds 64 KiB")
            with path.open("r", encoding="utf-8") as control_file:
                document = json.load(control_file)
            if not isinstance(document, dict) or set(document) - {"a", "b"}:
                raise ValueError("control root must contain only source keys a/b")
            pending = []
            for source in self.sources:
                patch = document.get(source.name, {})
                if not isinstance(patch, dict) or set(patch) - {"server_mode", "ack_mode", "delay_ms", "values", "poll_mode"}:
                    raise ValueError("invalid control fields for source %s" % source.name)
                settings = source.default_control()
                settings.update({key: value for key, value in patch.items() if key != "values"})
                if settings["server_mode"] not in ("running", "stopped"):
                    raise ValueError("server_mode must be running/stopped")
                if settings["ack_mode"] not in ("success", "drop", "bad"):
                    raise ValueError("ack_mode must be success/drop/bad")
                if settings["poll_mode"] not in ("respond", "drop"):
                    raise ValueError("poll_mode must be respond/drop")
                delay = settings["delay_ms"]
                if type(delay) is not int or not 0 <= delay <= 60000:
                    raise ValueError("delay_ms must be an integer from 0 to 60000")
                overrides = patch.get("values", {})
                if not isinstance(overrides, dict) or set(overrides) - {"1", "2"}:
                    raise ValueError("values must use device address keys 1/2")
                for address, value in overrides.items():
                    if type(value) is not int or not 0 <= value <= 255:
                        raise ValueError("device values must be integers from 0 to 255")
                    settings["values"][int(address)] = value
                pending.append((source, settings))
            for source, settings in pending:
                source.control = settings
            self.last_control_error = None
            self.event("control_applied", control=document)
            if self.state == "ready":
                self.manifest()
        except (OSError, ValueError, TypeError) as error:
            if str(error) != self.last_control_error:
                self.last_control_error = str(error)
                self.event("control_rejected", error=str(error))
                print("Control ignored, keeping last valid settings: %s" % error, file=sys.stderr, flush=True)

    def transmit(self, source, frame):
        try:
            count = os.write(source.master_fd, frame)
            if count != len(frame):
                raise OSError("short PTY write %d/%d" % (count, len(frame)))
            return True
        except OSError as error:
            source.io_errors += 1
            self.event("pty_write_failed", source=source.name, error=str(error), hex=hex_text(frame))
            return False

    def poll(self, source, frame):
        address = frame[0]
        source.polls[address] += 1
        source.last_poll_frames[address] = hex_text(frame)
        if source.control["poll_mode"] == "drop":
            return
        value = source.control["values"][address]
        # Rust parse_data_packet requires total_len == Len + 5. A configured
        # Len=1 therefore has two status bytes, and the point reads the first.
        response = checksum_frame(bytes([address, 0x08, 0x01, value, 0x00]))
        if self.transmit(source, response):
            source.responses[address] += 1
            source.last_response_frames[address] = hex_text(response)

    def command(self, source, frame):
        self.sequence += 1
        source.commands += 1
        sequence = self.sequence
        checksum_ok = len(frame) >= 3 and (sum(frame[:-1]) & 0xFF) == frame[-1]
        address = frame[0] if frame else None
        accepted = checksum_ok and address in (1, 2)
        mode = source.control["ack_mode"]
        delay = source.control["delay_ms"]
        event = {
            "at": timestamp(), "event": "command_received", "sequence": sequence,
            "source": source.name, "source_pid": source.child.pid, "port_id": str(source.alias),
            "device_address": address, "wire_hex": hex_text(frame),
            "body_hex": hex_text(frame[:-1]), "checksum_ok": checksum_ok,
            "accepted_by_fixture": accepted, "ack_mode": mode, "delay_ms": delay,
        }
        emit_json(self.command_log, event)
        if not accepted:
            source.invalid_frames += 1
            self.event("invalid_command", source=source.name, sequence=sequence, wire_hex=hex_text(frame))
            return
        if (mode == "drop" or source.stop_requested_at is not None
                or source.process_state not in ("starting", "running")):
            source.acks_dropped += 1
            emit_json(self.command_log, {"at": timestamp(), "event": "ack_dropped",
                                        "source": source.name, "sequence": sequence})
            return
        ack = ACK_SUCCESS if mode == "success" else b"\x00\x00"
        heapq.heappush(self.scheduled_acks, (time.monotonic() + delay / 1000.0,
                                          sequence, source, source.generation, ack))

    def consume(self, source, flush=False):
        # Poll packets have an exact seven-byte shape. Commands intentionally
        # use a different opcode. Structured commands carry their payload length
        # at byte 5 and can therefore be split immediately even when the next
        # 1 ms poll is already buffered. Arbitrary short commands retain the
        # idle-gap fallback below.
        while len(source.buffer) >= 7:
            candidate = bytes(source.buffer[:7])
            if (candidate[0] in (1, 2) and candidate[1:6] == POLL_PREFIX
                    and (sum(candidate[:-1]) & 0xFF) == candidate[-1]):
                del source.buffer[:7]
                self.poll(source, candidate)
                continue

            command_len = 7 + source.buffer[5]
            if command_len <= MAX_COMMAND_BYTES and len(source.buffer) >= command_len:
                candidate = bytes(source.buffer[:command_len])
                if ((sum(candidate[:-1]) & 0xFF) == candidate[-1]
                        and candidate[0] in (1, 2)):
                    del source.buffer[:command_len]
                    self.command(source, candidate)
                    continue
            break
        if flush and source.buffer:
            frame = bytes(source.buffer)
            source.buffer.clear()
            self.command(source, frame)

    def read_source(self, source):
        try:
            chunk = os.read(source.master_fd, 65536)
        except OSError as error:
            if error.errno in (errno.EAGAIN, errno.EWOULDBLOCK):
                return
            raise RuntimeError("PTY read failed for source %s: %s" % (source.name, error))
        if not chunk:
            return
        source.last_rx = time.monotonic()
        source.buffer.extend(chunk)
        if len(source.buffer) > MAX_COMMAND_BYTES:
            source.invalid_frames += 1
            self.event("oversized_frame_discarded", source=source.name, size=len(source.buffer))
            source.buffer.clear()
            return
        self.consume(source)

    def summary(self, force=False):
        now = time.monotonic()
        if not force and now - self.last_summary < SUMMARY_INTERVAL_SECONDS:
            return
        self.last_summary = now
        emit_json(self.poll_log, {
            "at": timestamp(), "event": "poll_summary", "state": self.state,
            "sources": {source.name: {
                "poll_counts": dict(source.polls), "response_counts": dict(source.responses),
                "last_poll_hex": source.last_poll_frames,
                "last_response_hex": source.last_response_frames,
                "values": source.control["values"], "commands": source.commands,
                "acks_sent": source.acks_sent, "acks_dropped": source.acks_dropped,
                "invalid_frames": source.invalid_frames, "io_errors": source.io_errors,
            } for source in self.sources},
        })

    def update_sources(self):
        """Non-blocking per-child lifecycle; never wait while another PTY polls."""
        now = time.monotonic()
        changed = False
        for source in self.sources:
            if source.stop_requested_at is not None:
                if source.child.poll() is None:
                    elapsed = now - source.stop_requested_at
                    if elapsed >= 6 and source.stop_escalation < 2:
                        source.child.kill()
                        source.stop_escalation = 2
                        self.event("source_stop_kill", source=source.name, pid=source.child.pid)
                    elif elapsed >= 4 and source.stop_escalation < 1:
                        source.child.terminate()
                        source.stop_escalation = 1
                        self.event("source_stop_terminate", source=source.name, pid=source.child.pid)
                    continue
                source.process_state = "stopped"
                source.stop_requested_at = None
                source.tcp_ready = False
                changed = True
                self.event("source_stopped", source=source.name, pid=source.child.pid,
                           exit_code=source.child.returncode)

            if source.control["server_mode"] == "stopped":
                if source.child.poll() is None:
                    source.stop_requested_at = now
                    source.process_state = "stopping"
                    source.stop_escalation = 0
                    source.stop()
                    retained = []
                    for scheduled in self.scheduled_acks:
                        if scheduled[2] is source:
                            source.acks_dropped += 1
                            emit_json(self.command_log, {"at": timestamp(), "event": "ack_cancelled_source_stop",
                                                        "source": source.name, "sequence": scheduled[1]})
                        else:
                            retained.append(scheduled)
                    self.scheduled_acks = retained
                    heapq.heapify(self.scheduled_acks)
                    changed = True
                    self.event("source_stop_requested", source=source.name, pid=source.child.pid)
                elif source.process_state != "stopped":
                    source.process_state = "stopped"
                    source.tcp_ready = False
                    changed = True
                continue

            if source.process_state == "stopped":
                source.start()
                changed = True
                self.event("source_restarted", source=source.name, pid=source.child.pid,
                           generation=source.generation)
            elif source.child.poll() is not None:
                raise RuntimeError("Rust source %s exited unexpectedly with %s; see %s" % (
                    source.name, source.child.returncode, source.log_dir / "console.log"))
            if source.process_state == "starting" and now - source.started_monotonic > self.args.startup_timeout:
                raise RuntimeError("source %s readiness timeout; inspect its logs and polls.jsonl" % source.name)
        if changed:
            self.manifest()

    def check_ready(self):
        now = time.monotonic()
        if now - self.last_ready_check < 0.5:
            return
        self.last_ready_check = now
        changed = False
        for source in self.sources:
            if source.process_state != "starting":
                continue
            if not source.tcp_ready:
                probe = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
                probe.settimeout(0.1)
                try:
                    source.tcp_ready = probe.connect_ex(("127.0.0.1", source.endpoint_port)) == 0
                finally:
                    probe.close()
            if source.tcp_ready and all(source.polls[address] > source.poll_baseline.get(address, 0)
                                        for address in (1, 2)):
                source.process_state = "running"
                changed = True
                self.event("source_ready", source=source.name, pid=source.child.pid, generation=source.generation)
        if self.state == "starting" and all(source.process_state == source.control["server_mode"]
                                              for source in self.sources):
            self.state = "ready"
            changed = True
            self.event("fixture_ready")
            print("Fixture ready: a=%d b=%d; logs=%s" % (
                self.args.port_a, self.args.port_b, self.root), flush=True)
        if changed:
            self.manifest()

    def run(self):
        self.initialize()
        while not self.stopping:
            self.load_control()
            self.update_sources()
            now = time.monotonic()
            for source in self.sources:
                if source.buffer and now - source.last_rx >= COMMAND_IDLE_SECONDS:
                    self.consume(source, flush=True)
            while self.scheduled_acks and self.scheduled_acks[0][0] <= now:
                _, sequence, source, generation, frame = heapq.heappop(self.scheduled_acks)
                if generation != source.generation or source.process_state not in ("starting", "running"):
                    source.acks_dropped += 1
                    emit_json(self.command_log, {"at": timestamp(), "event": "ack_discarded_old_generation",
                                                "source": source.name, "sequence": sequence})
                    continue
                sent = self.transmit(source, frame)
                if sent:
                    source.acks_sent += 1
                emit_json(self.command_log, {"at": timestamp(), "event": "ack_sent" if sent else "ack_write_failed",
                                            "source": source.name, "sequence": sequence, "wire_hex": hex_text(frame)})
            self.check_ready()
            self.summary()
            # A small select timeout also handles quiet command framing without
            # delaying the second source's polls while the first waits for ACK.
            for key, _ in self.selector.select(timeout=0.005):
                self.read_source(key.data)

    def close(self):
        for source in self.sources:
            source.stop()
        for source in self.sources:
            try:
                source.finish_stop()
            except Exception as error:
                self.event("cleanup_error", source=source.name, error=str(error))
                self.exit_code = 1
        self.selector.close()
        self.state = "stopped" if self.exit_code == 0 else "failed"
        self.summary(force=True)
        self.manifest()
        self.event("fixture_stopped", reason=self.exit_reason, exit_code=self.exit_code)
        self.command_log.close()
        self.poll_log.close()
        self.event_log.close()
        print("Fixture stopped: %s; logs retained at %s" % (self.exit_reason, self.root), flush=True)


def arguments():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--binary", required=True, type=Path, help="already compiled Rust rcd_server binary")
    parser.add_argument("--root", required=True, type=Path, help="new or empty isolated run directory")
    parser.add_argument("--port-a", type=int, default=4845)
    parser.add_argument("--port-b", type=int, default=4846)
    parser.add_argument("--startup-timeout", type=float, default=60.0)
    args = parser.parse_args()
    if not sys.platform.startswith("linux"):
        parser.error("this fixture is Linux-only; run it on the remote test machine")
    args.binary = args.binary.resolve(strict=True)
    args.root = args.root.resolve()
    if not args.binary.is_file():
        parser.error("--binary must be a regular file")
    if args.port_a == args.port_b or not all(1 <= port <= 65535 for port in (args.port_a, args.port_b)):
        parser.error("OPC ports must be distinct integers from 1 to 65535")
    if not 1 <= args.startup_timeout <= 600:
        parser.error("--startup-timeout must be 1..600 seconds")
    if args.root.exists() and (not args.root.is_dir() or any(args.root.iterdir())):
        parser.error("--root already contains files; select a new directory (nothing was removed)")
    args.root.mkdir(parents=True, exist_ok=True)
    return args


def main():
    args = arguments()
    fixture = Fixture(args)

    def stop(signum, _frame):
        fixture.stopping = True
        fixture.exit_reason = "signal_%s" % signum

    signal.signal(signal.SIGINT, stop)
    signal.signal(signal.SIGTERM, stop)
    try:
        fixture.run()
    except Exception as error:
        fixture.exit_code = 1
        fixture.exit_reason = str(error)
        fixture.event("fixture_failed", error=str(error))
        print("Fixture failed: %s" % error, file=sys.stderr, flush=True)
    finally:
        fixture.close()
    return fixture.exit_code


if __name__ == "__main__":
    sys.exit(main())
