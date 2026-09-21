"""Run bounded, read-only serial probe phases on explicitly supplied ports."""
import argparse
import collections
import json
import signal
import subprocess
import time
from pathlib import Path

from serial_parity_report import summarize

stop_requested = False


def stop(signum, frame):
    global stop_requested
    stop_requested = True


def progress(path):
    counts = collections.Counter()
    marked = 0
    if path.exists():
        for line in path.read_text().splitlines():
            try:
                row = json.loads(line)
            except json.JSONDecodeError:
                continue
            if row["type"] == "query":
                counts[row["result"]] += 1
                marked += bool(row["error_marks"])
    return dict(counts), marked


def main():
    global stop_requested
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--seconds", type=int, default=600)
    parser.add_argument("--variants", default="delay:raw,drain:raw,delay:marked,drain:marked")
    parser.add_argument("--prefix", default="ab")
    parser.add_argument("ports", nargs="+", help="/dev/ttyPORT=ID:LEN,ID:LEN,...")
    args = parser.parse_args()
    if not 1 <= args.seconds <= 3600:
        parser.error("seconds must be 1..3600")
    variants = [x.split(":") for x in args.variants.split(",")]
    if any(len(x) != 2 or x[0] not in ("delay", "drain") or x[1] not in ("raw", "marked") for x in variants):
        parser.error("invalid variants")
    ports = [x.split("=", 1) for x in args.ports]
    if any(len(x) != 2 for x in ports) or len({x[0] for x in ports}) != len(ports):
        parser.error("ports must be unique and include query specifications")
    signal.signal(signal.SIGINT, stop)
    signal.signal(signal.SIGTERM, stop)
    root = Path(__file__).resolve().parent
    for mode, rx_mode in variants:
        if stop_requested:
            break
        if subprocess.run(["pgrep", "-x", "rcd_server"], stdout=subprocess.DEVNULL).returncode == 0:
            raise SystemExit("Refusing to test while rcd_server is running")
        phase = f"{args.prefix}-{mode}-{rx_mode}"
        print("PHASE_START", phase, "seconds", args.seconds, flush=True)
        children = []
        started = time.monotonic()
        try:
            for port, query in ports:
                stem = f"{phase}-{Path(port).name}"
                log = root / f"{stem}.jsonl"
                error_log = root / f"{stem}.err"
                # Exclusive creation prevents accidentally replacing prior test evidence.
                with log.open("x") as output, error_log.open("x") as error:
                    process = subprocess.Popen(
                        ["timeout", "-k", "3s", f"{args.seconds + 10}s", str(root / "probe"),
                         port, "28800", str(args.seconds), "0", mode, rx_mode, query],
                        stdout=output, stderr=error, start_new_session=True)
                children.append((process, log))
            last_update = 0
            while any(p.poll() is None for p, _ in children) and not stop_requested:
                if subprocess.run(["pgrep", "-x", "rcd_server"], stdout=subprocess.DEVNULL).returncode == 0:
                    print("ABORT: rcd_server started during the test; releasing ports", flush=True)
                    stop_requested = True
                    break
                elapsed = time.monotonic() - started
                if elapsed - last_update >= 30:
                    print("PROGRESS", phase, round(elapsed), "s", flush=True)
                    for _, path in children:
                        counts, marked = progress(path)
                        print(path.stem, counts, "marked", marked, flush=True)
                    last_update = elapsed
                time.sleep(1)
        finally:
            for process, _ in children:
                if process.poll() is None:
                    process.terminate()
            for process, log in children:
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    import os
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait(timeout=5)
                print("EXIT", log.name, process.returncode, flush=True)
                summarize(log)
        print("PHASE_END", phase, round(time.monotonic() - started), "s", flush=True)
    print("RUN_END", "interrupted" if stop_requested else "complete", flush=True)


if __name__ == "__main__":
    main()
