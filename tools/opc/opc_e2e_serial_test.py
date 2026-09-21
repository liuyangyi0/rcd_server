#!/usr/bin/env python3
"""
End-to-end OPC UA to serial test for rcd_server.

This script builds/starts a temporary rcd_server instance with a minimal config,
writes to the OPC UA "<COM>_Send" node, and optionally verifies the exact bytes
from a second serial port.
"""

from __future__ import annotations

import argparse
import shutil
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from typing import Iterable, Optional


ROOT = Path(__file__).resolve().parents[2]
PYTHON = sys.executable
OPC_CLIENT = Path(__file__).resolve().with_name("opc_client_test.py")


def run_checked(args: list[str], cwd: Path, timeout: float = 120.0) -> subprocess.CompletedProcess:
    return subprocess.run(
        args,
        cwd=str(cwd),
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        timeout=timeout,
        check=True,
    )


def build_server(skip_build: bool) -> Path:
    exe = ROOT / "target" / "debug" / ("rcd_server.exe" if sys.platform == "win32" else "rcd_server")
    if not skip_build or not exe.exists():
        print("BUILD: cargo build")
        result = run_checked(["cargo", "build"], ROOT, timeout=240.0)
        if result.stdout.strip():
            print(result.stdout)
    if not exe.exists():
        raise FileNotFoundError(f"server executable not found: {exe}")
    return exe


def write_text(path: Path, content: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8")


def create_minimal_config(
    config_dir: Path,
    server_serial: str,
    baudrate: int,
    is_special: bool,
    timeout_ms: int,
) -> None:
    write_text(
        config_dir / "config.toml",
        f"""[server]
primary_ip = "127.0.0.1:11001"
secondary_ip = "127.0.0.1:11002"
run_on = "secondary"
current_run = "secondary"

[serial]
baud_rate = {baudrate}
data_bits = "Eight"
parity = "None"
stop_bits = "One"
flow_control = "none"

[settings]
timeout = {timeout_ms}
poll_interval_ms = 20
command_retry_count = 1

[logging]
level = "info"
directory = "logs"
basename = "rcd_server_e2e"
duplicate_to_stderr = "info"
rotate_age = "none"
rotate_size_bytes = 104857600
keep_files = 3
write_mode = "direct"
serial_frame_logs = true
opcua_transport_errors = false
""",
    )
    write_text(config_dir / "calc.toml", "rules = []\n")
    write_text(
        config_dir / "rcd_e2e.csv",
        f"""com,{server_serial},,,,,,,
baud_rate,{baudrate},,,,,,,
is_special,{str(is_special).lower()},,,,,,,
com_index,1,,,,,,,
device_id,1,,,,,,,
data_len,1,,,,,,,
kks_prefix,E2E_,,,,,,,
kks,type_,f_type,byte_index,bit_index,def,max,min,lh
Status,uint,status,1,0.7,0,255,0,0
""",
    )


def wait_for_opc(endpoint: str, timeout: float) -> tuple[bool, str]:
    deadline = time.monotonic() + timeout
    last_output = ""
    while time.monotonic() < deadline:
        proc = subprocess.run(
            [
                PYTHON,
                str(OPC_CLIENT),
                "--endpoint",
                endpoint,
                "--check-types",
                "--expect-send-value",
                "IDLE",
                "--quality-node",
                "E2E_Status",
                "--timeout",
                "2",
            ],
            cwd=str(ROOT),
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            timeout=5,
        )
        last_output = proc.stdout.strip()
        if proc.returncode == 0:
            print("OPC READY:")
            print(last_output)
            return True, last_output
        time.sleep(0.5)
    return False, last_output


def terminate(process: subprocess.Popen[str]) -> str:
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)
    output = ""
    if process.stdout is not None:
        try:
            output = process.stdout.read()
        except Exception:
            output = ""
    return output


def parse_args(argv: Iterable[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Run automated OPC UA to serial E2E test.")
    parser.add_argument("--server-serial", required=True, help="serial port opened by rcd_server, for example COM21")
    parser.add_argument("--monitor-serial", help="peer serial port used to read bytes and send ACK")
    parser.add_argument("--baud", type=int, default=28800)
    parser.add_argument("--hex", default="010602", help="command body without checksum")
    parser.add_argument("--endpoint", default="opc.tcp://127.0.0.1:4841/")
    parser.add_argument("--startup-timeout", type=float, default=20.0)
    parser.add_argument("--serial-timeout", type=float, default=5.0)
    parser.add_argument("--server-timeout-ms", type=int, default=1000)
    parser.add_argument("--skip-build", action="store_true")
    parser.add_argument(
        "--mark-space",
        action="store_true",
        help="use production Mark/Space split write mode instead of direct special-port mode",
    )
    parser.add_argument(
        "--keep-temp",
        action="store_true",
        help="keep temporary server directory after the test for log inspection",
    )
    return parser.parse_args(list(argv))


def main(argv: Iterable[str]) -> int:
    args = parse_args(argv)
    server_exe = build_server(args.skip_build)

    temp_dir = Path(tempfile.mkdtemp(prefix="rcd_opc_e2e_"))
    try:
        temp_exe = temp_dir / server_exe.name
        shutil.copy2(server_exe, temp_exe)
        create_minimal_config(
            temp_dir / "config",
            args.server_serial,
            args.baud,
            is_special=not args.mark_space,
            timeout_ms=args.server_timeout_ms,
        )

        print(f"SERVER DIR: {temp_dir}")
        print(f"SERVER SERIAL: {args.server_serial}")
        if args.monitor_serial:
            print(f"MONITOR SERIAL: {args.monitor_serial}")
        else:
            print("MONITOR SERIAL: <none>; this can test OPC acceptance but cannot prove bytes on the wire")

        process = subprocess.Popen(
            [str(temp_exe)],
            cwd=str(temp_dir),
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            bufsize=1,
        )
        try:
            ready, output = wait_for_opc(args.endpoint, args.startup_timeout)
            if not ready:
                print("OPC READY: failed")
                if output:
                    print(output)
                return 1

            command = [
                PYTHON,
                str(OPC_CLIENT),
                "--endpoint",
                args.endpoint,
                "--port",
                args.server_serial,
                "--hex",
                args.hex,
                "--status-timeout",
                str(max(2.0, args.serial_timeout + 2.0)),
            ]
            if args.monitor_serial:
                command.extend(
                    [
                        "--expect-status",
                        "received",
                        "--serial-monitor",
                        args.monitor_serial,
                        "--serial-baud",
                        str(args.baud),
                        "--serial-timeout",
                        str(args.serial_timeout),
                        "--serial-reply-hex",
                        "005050",
                    ]
                )
            else:
                command.extend(["--expect-status", "any"])

            test = subprocess.run(
                command,
                cwd=str(ROOT),
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                timeout=max(15.0, args.serial_timeout + 10.0),
            )
            print(test.stdout)
            if test.returncode != 0:
                return test.returncode

            method_test = subprocess.run(
                [
                    PYTHON,
                    str(OPC_CLIENT),
                    "--endpoint",
                    args.endpoint,
                    "--port",
                    args.server_serial,
                    "--hex",
                    args.hex,
                    "--mode",
                    "method",
                ],
                cwd=str(ROOT),
                text=True,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                timeout=15,
            )
            print(method_test.stdout)
            return method_test.returncode
        finally:
            server_output = terminate(process)
            if server_output.strip():
                print("SERVER OUTPUT:")
                print(server_output[-4000:])
    finally:
        if args.keep_temp:
            print(f"TEMP KEPT: {temp_dir}")
        else:
            shutil.rmtree(temp_dir, ignore_errors=True)


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
