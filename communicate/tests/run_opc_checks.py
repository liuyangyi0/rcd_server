#!/usr/bin/env python3
"""Software-only OPC checks on 46. No serial ports, service units or live APIs.

Run after building the Rust binary/example and opc_probe.go. An existing, empty
scratch directory is required; evidence is intentionally retained on failure.
"""
import argparse
import json
from pathlib import Path
import signal
import socket
import subprocess
import time


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
        if port < 10000:
            raise RuntimeError("OS selected a non-isolated port")
        return port


def wait_ready(process, port):
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError(f"server exited early: {process.returncode}")
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.2):
                return
        except OSError:
            time.sleep(0.1)
    raise RuntimeError("isolated OPC endpoint did not start")


def run_probe(probe, root, port, security, mode):
    result = subprocess.run(
        [str(probe), f"opc.tcp://127.0.0.1:{port}/", security, mode],
        capture_output=True, text=True, timeout=35,
    )
    (root / f"probe-{security}.log").write_text(result.stdout + result.stderr)
    if result.returncode:
        raise RuntimeError(f"{mode}/{security} failed: {result.stderr}")
    report = json.loads(result.stdout.strip().splitlines()[-1])
    if not report.get("passed") or report.get("hardware_used"):
        raise RuntimeError("invalid probe report")
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--probe", type=Path, required=True)
    parser.add_argument("--scratch", type=Path, required=True)
    args = parser.parse_args()
    source, probe, scratch = (v.resolve(strict=True) for v in (args.source, args.probe, args.scratch))
    if any(scratch.iterdir()) or scratch == source or source in scratch.parents:
        raise RuntimeError("scratch must be empty and outside the source directory")
    template = (source / "examples/config.toml").read_text()
    if 'bind_ip = "127.0.0.1"' not in template or "enabled = true" in template:
        raise RuntimeError("example is not the disabled loopback fixture")
    reports = []
    for mode in ("pending", "sequence"):
        root = scratch / mode
        root.mkdir()
        port = free_port()
        (root / "config.toml").write_text(template.replace("port = 14842", f"port = {port}"))
        (root / "points.csv").write_bytes((source / "examples/points.csv").read_bytes())
        if mode == "pending":
            argv = [str(source / "target/debug/rcd_communicate"), "run", "--config", str(root / "config.toml"),
                    "--state-dir", str(root / "state"), "--pki-dir", str(root / "pki"), "--log-dir", str(root / "logs")]
        else:
            argv = [str(source / "target/debug/examples/opc_fixture"), str(root)]
        with (root / "server.log").open("w") as log:
            process = subprocess.Popen(argv, cwd=root, stdout=log, stderr=subprocess.STDOUT)
            try:
                wait_ready(process, port)
                for security in ("none", "secure"):
                    reports.append(run_probe(probe, root, port, security, mode))
                if mode == "pending":
                    duplicate = subprocess.run(argv, cwd=root, capture_output=True, text=True, timeout=10)
                    (root / "duplicate.log").write_text(duplicate.stdout + duplicate.stderr)
                    if duplicate.returncode == 0 or "state directory already in use" not in duplicate.stderr:
                        raise RuntimeError("duplicate process did not reject the instance lock")
                    process.send_signal(signal.SIGTERM)
                if process.wait(timeout=55) != 0:
                    raise RuntimeError(f"server did not exit cleanly: {process.returncode}")
            finally:
                # Only the exact software-fixture child created above is stopped.
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait(timeout=5)
    result = {"passed": True, "hardware_used": False, "reports": reports,
              "instance_lock": "passed", "sigterm_shutdown": "passed"}
    (scratch / "report.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result))


if __name__ == "__main__":
    main()
