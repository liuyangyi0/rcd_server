"""Bounded READ_STATUS experiments; never changes gateway configuration or drivers."""
import argparse
import collections
import csv
import datetime
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import time

from serial_parity_report import summarize

stop_requested = False
PHASES = {
    "baseline": ("delay", "raw", {}),
    "drain": ("drain", "raw", {}),
    "flush": ("flush", "raw", {}),
    "marked": ("delay", "marked", {}),
    "nul": ("delay", "nul", {}),
    "ignore": ("delay", "ignore", {}),
    "discard": ("delay", "discard", {}),
    "noclear": ("delay", "raw", {"PROBE_CLEAR_INPUT": "0"}),
    "gap100": ("delay", "raw", {"PROBE_GAP_MS": "100"}),
    "gap220": ("delay", "raw", {"PROBE_GAP_MS": "220"}),
    "address2": ("delay", "raw", {"PROBE_ADDRESS_MARGIN_MS": "2"}),
    "earlyread": ("delay", "raw", {"PROBE_PAYLOAD_MARGIN_MS": "0"}),
    "pre0": ("delay", "raw", {"PROBE_PRE_MS": "0"}),
    "timeout600": ("delay", "raw", {"PROBE_TIMEOUT_MS": "600"}),
    "reverse": ("delay", "raw", {}),
}


def request_stop(signum, frame):
    global stop_requested
    stop_requested = True


def timestamp():
    return datetime.datetime.now().astimezone().isoformat()


def gateway_running():
    result = subprocess.run(["pgrep", "-x", "rcd_server"], capture_output=True, text=True)
    if result.returncode not in (0, 1):
        raise RuntimeError("Cannot establish whether rcd_server is running")
    return result.returncode == 0


def config_digest(folder):
    digest = hashlib.sha256()
    for path in sorted(list(folder.glob("*.csv")) + list(folder.glob("*.toml"))):
        digest.update(path.name.encode())
        digest.update(path.read_bytes())
    return digest.hexdigest()


def load_ports(folder):
    ports = collections.defaultdict(list)
    for path in sorted(folder.glob("*.csv")):
        with path.open(encoding="utf-8-sig", newline="") as stream:
            rows = list(csv.reader(stream))
        header = {r[0].strip().lower(): r[1].strip() for r in rows[:7] if len(r) >= 2}
        port = header["com"]
        if not port.startswith("/dev/tty"):
            raise ValueError(f"Unexpected port {port}")
        device = int(header["deviceid"])
        length = int(header["datalen"])
        baud = int(header["baudrate"])
        if not 0 <= device <= 255 or not 0 < length <= 255 or not 0 < baud <= 4000000:
            raise ValueError(f"Invalid configuration in {path.name}")
        ports[port].append((device, length, baud))
    for port, entries in ports.items():
        if len({x[2] for x in entries}) != 1 or len({x[0] for x in entries}) != len(entries):
            raise ValueError(f"Inconsistent baud or duplicate device on {port}")
        entries.sort()
    return dict(sorted(ports.items()))


def proc_snapshot():
    path = "/proc/tty/driver/pci_ch35_38x"
    try:
        return Path(path).read_text()
    except PermissionError:
        result = subprocess.run(["sudo", "-n", "cat", path], capture_output=True, text=True, timeout=3)
        return result.stdout if result.returncode == 0 else "unavailable: " + result.stderr.strip()
    except OSError as error:
        return "unavailable: " + str(error)


def progress(path):
    counts = collections.Counter()
    marks = 0
    for line in path.read_text().splitlines():
        try:
            row = json.loads(line)
        except json.JSONDecodeError:
            continue
        if row.get("type") == "query":
            counts[row["result"]] += 1
            marks += bool(row["error_marks"])
    return dict(counts), marks


def main():
    global stop_requested
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config-dir", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--probe", type=Path, required=True)
    parser.add_argument("--prefix", required=True)
    parser.add_argument("--phases", default="baseline,drain,flush,marked")
    parser.add_argument("--seconds", type=int, default=120)
    parser.add_argument("--ports", help="Comma-separated tty names, e.g. ttyWCH0,ttyWCH4")
    parser.add_argument("--device", type=int, help="Only this configured device on the selected ports")
    args = parser.parse_args()
    phase_names = args.phases.split(",")
    if not 1 <= args.seconds <= 3600 or not phase_names or any(x not in PHASES for x in phase_names):
        parser.error("invalid phases or duration")
    if not args.prefix or any(x not in "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-" for x in args.prefix):
        parser.error("unsafe prefix")
    if len(phase_names) != len(set(phase_names)):
        parser.error("duplicate phases")
    ports = load_ports(args.config_dir)
    if args.ports:
        requested = {"/dev/" + name for name in args.ports.split(",")}
        if requested - ports.keys():
            parser.error("some requested ports are not configured")
        ports = {p: d for p, d in ports.items() if p in requested}
    if args.device is not None:
        ports = {p: [x for x in d if x[0] == args.device] for p, d in ports.items()}
        ports = {p: d for p, d in ports.items() if d}
    if not ports:
        parser.error("no devices")
    args.output.mkdir(parents=True, exist_ok=True)
    args.probe = args.probe.resolve(strict=True)
    original_digest = config_digest(args.config_dir)
    metadata = {
        "started": timestamp(), "uname": list(os.uname()), "config_sha256": original_digest,
        "probe_sha256": hashlib.sha256(args.probe.read_bytes()).hexdigest(),
        "ports": ports, "phases": phase_names, "seconds_per_phase": args.seconds,
        "base_options": {"PROBE_GAP_MS": "20"}, "phase_definitions": PHASES,
    }
    with (args.output / (args.prefix + "-metadata.json")).open("x") as stream:
        json.dump(metadata, stream, indent=2)
    signal.signal(signal.SIGINT, request_stop)
    signal.signal(signal.SIGTERM, request_stop)
    events_path = args.output / (args.prefix + "-events.jsonl")
    with events_path.open("x") as events:
        def event(kind, **fields):
            row = {"time": timestamp(), "event": kind, **fields}
            events.write(json.dumps(row) + "\n")
            events.flush()
            print(kind, fields.get("phase", ""), fields.get("reason", ""), flush=True)

        for name in phase_names:
            if stop_requested:
                break
            if gateway_running() or config_digest(args.config_dir) != original_digest:
                event("ABORT", reason="gateway running or configuration changed")
                stop_requested = True
                break
            mode, rx, overrides = PHASES[name]
            children = []
            started = time.monotonic()
            event("PHASE_START", phase=name, proc=proc_snapshot())
            try:
                for port, entries in ports.items():
                    selected = list(reversed(entries)) if name == "reverse" else entries
                    query = ",".join(f"{device}:{length}" for device, length, _ in selected)
                    env = {k: v for k, v in os.environ.items() if not k.startswith("PROBE_")}
                    env.update(PROBE_GAP_MS="20")
                    env.update(overrides)
                    stem = f"{args.prefix}-{name}-{Path(port).name}"
                    log = args.output / (stem + ".jsonl")
                    with log.open("x") as output, (args.output / (stem + ".err")).open("x") as errors:
                        process = subprocess.Popen(
                            ["timeout", "-k", "3s", f"{args.seconds + 12}s", str(args.probe),
                             port, str(entries[0][2]), str(args.seconds), "0", mode, rx, query],
                            stdout=output, stderr=errors, env=env, start_new_session=True)
                    children.append((process, log))
                last_progress = started
                while any(p.poll() is None for p, _ in children) and not stop_requested:
                    if gateway_running() or config_digest(args.config_dir) != original_digest:
                        event("ABORT", phase=name, reason="gateway started or configuration changed during test")
                        stop_requested = True
                        break
                    if any(p.poll() not in (None, 0) for p, _ in children):
                        event("ABORT", phase=name, reason="probe failed; inspect stderr before continuing")
                        stop_requested = True
                        break
                    now = time.monotonic()
                    if now - last_progress >= 30:
                        results = {log.stem: progress(log) for _, log in children}
                        event("PROGRESS", phase=name, elapsed_s=round(now - started), results=results)
                        print(results, flush=True)
                        last_progress = now
                    time.sleep(1)
            finally:
                for process, _ in children:
                    if process.poll() is None:
                        os.killpg(process.pid, signal.SIGTERM)
                for process, log in children:
                    try:
                        process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        os.killpg(process.pid, signal.SIGKILL)
                        process.wait(timeout=5)
                    print("EXIT", log.name, process.returncode, flush=True)
                    summarize(log)
                    if process.returncode:
                        stop_requested = True
                event("PHASE_END", phase=name, elapsed_s=round(time.monotonic() - started), proc=proc_snapshot())
        event("RUN_END", reason="interrupted" if stop_requested else "complete")
    return 1 if stop_requested else 0


if __name__ == "__main__":
    raise SystemExit(main())
