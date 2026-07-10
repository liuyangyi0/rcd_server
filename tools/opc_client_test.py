#!/usr/bin/env python3
"""
OPC UA command-write test client for rcd_server.

It writes a hex command string to a "<port>_Send" OPC UA node and can
optionally monitor a serial port to verify the actual bytes written.
The server appends the one-byte checksum itself, so pass only the command body.
"""

from __future__ import annotations

import argparse
import re
import sys
import threading
import time
from dataclasses import dataclass
from typing import Iterable, Optional
from urllib.parse import urlparse


STATUS_TEXT = {
    "": "idle",
    "0": "idle",
    "IDLE": "idle",
    "1": "received",
    "2": "waiting",
    "3": "failed",
}


@dataclass
class SerialCapture:
    data: bytes
    error: Optional[str] = None


def import_opcua():
    try:
        from opcua import Client, ua
    except ImportError as exc:
        raise SystemExit(
            "Missing dependency: opcua. Install with:\n"
            "  python -m pip install -r tools/requirements.txt"
        ) from exc
    return Client, ua


def normalize_hex(value: str) -> str:
    cleaned = re.sub(r"\s+", "", value.strip())
    if cleaned.lower().startswith("0x"):
        cleaned = cleaned[2:]
    if len(cleaned) < 4:
        raise ValueError("hex command must contain at least 2 bytes")
    if len(cleaned) % 2 != 0:
        raise ValueError("hex command length must be even")
    if not re.fullmatch(r"[0-9a-fA-F]+", cleaned):
        raise ValueError("hex command contains non-hex characters")
    return cleaned.upper()


def bytes_from_hex(value: str) -> bytes:
    return bytes.fromhex(normalize_hex(value))


def frame_with_checksum(command_hex: str) -> bytes:
    body = bytes_from_hex(command_hex)
    return body + bytes([sum(body) & 0xFF])


def format_hex(data: bytes) -> str:
    return " ".join(f"{b:02X}" for b in data)


def status_label(value: object) -> str:
    text = "" if value is None else str(value)
    mapped = STATUS_TEXT.get(text, "unknown")
    return f"{text!r} ({mapped})"


def opc_port_stem(port: str) -> str:
    """Match the server's normalization of Linux and Windows serial paths."""
    segments = [segment for segment in re.split(r"[/\\]+", port) if segment]
    return segments[-1] if segments else port


def read_serial_bytes(
    port: str,
    baudrate: int,
    timeout: float,
    expected_len: int,
    reply: bytes,
) -> SerialCapture:
    try:
        import serial
    except ImportError as exc:
        return SerialCapture(
            b"",
            "Missing dependency: pyserial. Install with: python -m pip install -r tools/requirements.txt",
        )

    captured = bytearray()
    deadline = time.monotonic() + timeout
    try:
        with serial.Serial(port=port, baudrate=baudrate, timeout=0.05) as ser:
            ser.reset_input_buffer()
            while time.monotonic() < deadline and len(captured) < expected_len:
                chunk = ser.read(max(1, expected_len - len(captured)))
                if chunk:
                    captured.extend(chunk)
            if reply and len(captured) >= expected_len:
                ser.write(reply)
                ser.flush()
    except Exception as exc:  # noqa: BLE001 - command-line diagnostics should show device errors
        return SerialCapture(bytes(captured), str(exc))

    return SerialCapture(bytes(captured))


def start_serial_monitor(
    port: Optional[str],
    baudrate: int,
    timeout: float,
    expected_len: int,
    reply: bytes,
) -> tuple[Optional[threading.Thread], dict[str, SerialCapture]]:
    if not port:
        return None, {}

    result: dict[str, SerialCapture] = {}

    def run() -> None:
        result["capture"] = read_serial_bytes(port, baudrate, timeout, expected_len, reply)

    thread = threading.Thread(target=run, daemon=True)
    thread.start()
    time.sleep(0.2)
    return thread, result


def browse_send_nodes(root, ua, namespace_index: int, max_depth: int = 8) -> list[str]:
    found: list[str] = []

    def walk(node, depth: int) -> None:
        if depth > max_depth:
            return
        try:
            children = node.get_children()
        except Exception:
            return
        for child in children:
            try:
                browse_name = child.get_browse_name()
                node_class = child.get_node_class()
            except Exception:
                continue
            if (
                child.nodeid.NamespaceIndex == namespace_index
                and browse_name.Name.endswith("_Send")
                and node_class == ua.NodeClass.Variable
            ):
                found.append(browse_name.Name)
            walk(child, depth + 1)

    walk(root, 0)
    return sorted(set(found))


def wait_for_status(
    node,
    timeout: float,
    poll_interval: float,
    stop_codes: set[str],
) -> object:
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            last = node.get_value()
        except Exception:
            return last
        code = str(last)
        if code in stop_codes:
            return last
        if code in {"1", "3"}:
            return last
        time.sleep(poll_interval)
    return last


def connect_client(args):
    Client, ua = import_opcua()
    client = Client(args.endpoint, timeout=args.timeout)
    if args.username:
        client.set_user(args.username)
        client.set_password(args.password or "")
    client.connect()
    return client, ua


def node_for_send_name(client, ua, namespace_index: int, send_name: str):
    return client.get_node(ua.NodeId(send_name, namespace_index))


def format_access_levels(levels: set[object]) -> str:
    if not levels:
        return "<none>"
    return " | ".join(sorted(getattr(level, "name", str(level)) for level in levels))


def inspect_send_node(
    client,
    ua,
    namespace_index: int,
    send_name: str,
    expected_value: Optional[str] = None,
) -> bool:
    """Read and strictly validate the OPC UA attributes of one send node."""
    node = node_for_send_name(client, ua, namespace_index, send_name)
    node_class = node.get_node_class()
    browse_name = node.get_browse_name()
    data_type = node.get_data_type()
    variant_type = node.get_data_type_as_variant_type()
    value_rank = node.get_value_rank()
    value = node.get_value()
    type_definition = node.get_type_definition()
    access_level = node.get_access_level()
    user_access_level = node.get_user_access_level()

    print(f"NODE: {send_name}")
    print(f"  NodeId: {node.nodeid}")
    print(f"  BrowseName: {browse_name}")
    print(f"  NodeClass: {node_class.name}")
    print(f"  DataType: {data_type} ({variant_type.name})")
    print(f"  Value: {value!r} (Python type={type(value).__name__})")
    print(f"  ValueRank: {value_rank}")
    print(f"  TypeDefinition: {type_definition}")
    print(f"  AccessLevel: {format_access_levels(access_level)}")
    print(f"  UserAccessLevel: {format_access_levels(user_access_level)}")

    expected_string = ua.NodeId(ua.ObjectIds.String)
    expected_type_definition = ua.NodeId(ua.ObjectIds.BaseDataVariableType)
    expected_node_id = ua.NodeId(send_name, namespace_index)
    failures: list[str] = []
    if browse_name.NamespaceIndex != namespace_index:
        failures.append(
            "BrowseName namespace expected "
            f"{namespace_index}, got {browse_name.NamespaceIndex}"
        )
    if browse_name.Name != send_name:
        failures.append(f"BrowseName expected {send_name!r}, got {browse_name.Name!r}")
    if node_class != ua.NodeClass.Variable:
        failures.append(f"NodeClass expected Variable, got {node_class.name}")
    if data_type != expected_string:
        failures.append(f"DataType expected {expected_string} (String), got {data_type}")
    if variant_type != ua.VariantType.String:
        failures.append(f"VariantType expected String, got {variant_type.name}")
    if not isinstance(value, str):
        failures.append(f"Value expected str, got {type(value).__name__}")
    elif not value:
        failures.append("Value expected a non-empty compatibility string")
    elif expected_value is not None and value != expected_value:
        failures.append(f"Value expected {expected_value!r}, got {value!r}")
    if value_rank != -1:
        failures.append(f"ValueRank expected -1 (Scalar), got {value_rank}")
    if type_definition != expected_type_definition:
        failures.append(
            "TypeDefinition expected "
            f"{expected_type_definition} (BaseDataVariableType), got {type_definition}"
        )
    if ua.AccessLevel.CurrentRead not in access_level:
        failures.append("AccessLevel is missing CurrentRead")
    if ua.AccessLevel.CurrentWrite not in access_level:
        failures.append("AccessLevel is missing CurrentWrite")
    if ua.AccessLevel.CurrentRead not in user_access_level:
        failures.append("UserAccessLevel is missing CurrentRead")
    if ua.AccessLevel.CurrentWrite not in user_access_level:
        failures.append("UserAccessLevel is missing CurrentWrite")

    browse_path = [f"{namespace_index}:Devices", f"{namespace_index}:{send_name}"]
    try:
        browse_path_node = client.get_objects_node().get_child(browse_path)
        if browse_path_node.nodeid != expected_node_id:
            failures.append(
                f"Qualified BrowsePath resolved to {browse_path_node.nodeid}, "
                f"expected {expected_node_id}"
            )
        else:
            print(f"  QualifiedBrowsePath: {'/'.join(browse_path)} (PASS)")
    except Exception as exc:  # noqa: BLE001 - report the OPC status in CLI diagnostics
        failures.append(f"Qualified BrowsePath {'/'.join(browse_path)} failed: {exc}")

    if failures:
        print("  TYPE CHECK: FAIL")
        for failure in failures:
            print(f"    - {failure}")
        return False

    print("  TYPE CHECK: PASS (writable scalar String)")
    return True


def inspect_server_metadata(client, ua, namespace_index: int, send_names: list[str]) -> bool:
    failures: list[str] = []
    endpoints = client.get_endpoints()
    for endpoint in endpoints:
        endpoint_host = urlparse(endpoint.EndpointUrl).hostname
        discovery_hosts = [urlparse(url).hostname for url in endpoint.Server.DiscoveryUrls or []]
        if endpoint_host in {"0.0.0.0", "::", None}:
            failures.append(f"EndpointUrl is not client-accessible: {endpoint.EndpointUrl}")
        if any(host in {"0.0.0.0", "::", None} for host in discovery_hosts):
            failures.append(
                f"DiscoveryUrls contain a wildcard or invalid host: {endpoint.Server.DiscoveryUrls}"
            )
        if not endpoint.Server.ProductUri:
            failures.append("ApplicationDescription.ProductUri is empty")

    methods_node = client.get_node(ua.NodeId("Methods", namespace_index))
    methods_type = methods_node.get_type_definition()
    expected_object_type = ua.NodeId(ua.ObjectIds.BaseObjectType)
    if methods_type != expected_object_type:
        failures.append(
            f"Methods TypeDefinition expected {expected_object_type}, got {methods_type}"
        )

    for send_name in send_names:
        port = send_name[: -len("_Send")]
        method_name = f"{port}_SendCommand"
        method = client.get_node(ua.NodeId(method_name, namespace_index))
        executable = method.get_attribute(ua.AttributeIds.Executable).Value.Value
        user_executable = method.get_attribute(ua.AttributeIds.UserExecutable).Value.Value
        if executable is not True:
            failures.append(f"{method_name}.Executable expected True, got {executable!r}")
        if user_executable is not True:
            failures.append(
                f"{method_name}.UserExecutable expected True, got {user_executable!r}"
            )

        for suffix, expected_browse_name in [
            ("Input", "InputArguments"),
            ("Output", "OutputArguments"),
        ]:
            argument = client.get_node(
                ua.NodeId(f"{method_name}_{suffix}", namespace_index)
            )
            browse_name = argument.get_browse_name()
            type_definition = argument.get_type_definition()
            expected_property_type = ua.NodeId(ua.ObjectIds.PropertyType)
            if browse_name.NamespaceIndex != 0 or browse_name.Name != expected_browse_name:
                failures.append(
                    f"{method_name}_{suffix} BrowseName expected "
                    f"0:{expected_browse_name}, got {browse_name}"
                )
            if type_definition != expected_property_type:
                failures.append(
                    f"{method_name}_{suffix} TypeDefinition expected "
                    f"{expected_property_type}, got {type_definition}"
                )

    print(f"SERVER METADATA: endpoints={len(endpoints)}, methods={len(send_names)}")
    if failures:
        print("SERVER METADATA CHECK: FAIL")
        for failure in failures:
            print(f"  - {failure}")
        return False
    print("SERVER METADATA CHECK: PASS")
    return True


def inspect_unavailable_data_quality(client, ua, namespace_index: int, node_name: str) -> bool:
    node = client.get_node(ua.NodeId(node_name, namespace_index))
    read_value = ua.ReadValueId()
    read_value.NodeId = node.nodeid
    read_value.AttributeId = ua.AttributeIds.Value
    read_params = ua.ReadParameters()
    read_params.NodesToRead.append(read_value)
    # Node.get_data_value 会对 Bad 质量直接抛异常；底层 Read 可保留状态和值供诊断。
    data_value = client.uaclient.read(read_params)[0]
    value = data_value.Value.Value
    status = data_value.StatusCode
    print(f"QUALITY NODE: {node_name} value={value!r}, status={status.name}")
    if status.is_good():
        print("QUALITY CHECK: FAIL (offline/unavailable value is marked Good)")
        return False
    if value is not None:
        print("QUALITY CHECK: FAIL (Bad quality must not expose a synthetic zero value)")
        return False
    print("QUALITY CHECK: PASS")
    return True


def call_send_method(client, ua, namespace_index: int, port: str, command_hex: str) -> object:
    methods_node = client.get_node(ua.NodeId("Methods", namespace_index))
    method_node = client.get_node(ua.NodeId(f"{port}_SendCommand", namespace_index))
    return methods_node.call_method(
        method_node.nodeid,
        ua.Variant(command_hex, ua.VariantType.String),
    )


def validate_serial_capture(capture: SerialCapture, expected: bytes) -> bool:
    if capture.error:
        print(f"SERIAL ERROR: {capture.error}")
        if capture.data:
            print(f"SERIAL PARTIAL: {format_hex(capture.data)}")
        return False

    print(f"SERIAL READ: {format_hex(capture.data) if capture.data else '<none>'}")
    print(f"SERIAL EXPECTED: {format_hex(expected)}")
    if capture.data == expected:
        print("SERIAL RESULT: OK")
        return True

    print("SERIAL RESULT: MISMATCH")
    return False


def parse_args(argv: Iterable[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Write a command to rcd_server OPC UA <port>_Send node."
    )
    parser.add_argument("--endpoint", default="opc.tcp://127.0.0.1:4840/")
    parser.add_argument("--namespace-uri", default="urn:SerialServer")
    parser.add_argument(
        "--port",
        help="rcd_server port name or path, for example COM3 or /dev/ttyAP0",
    )
    parser.add_argument("--node", help="explicit send node name, for example COM3_Send")
    parser.add_argument("--hex", default="010602", help="hex command without checksum")
    parser.add_argument(
        "--mode",
        choices=("write", "method"),
        default="write",
        help="write <port>_Send or call <port>_SendCommand",
    )
    parser.add_argument("--username")
    parser.add_argument("--password")
    parser.add_argument("--timeout", type=float, default=5.0)
    parser.add_argument("--status-timeout", type=float, default=5.0)
    parser.add_argument("--poll-interval", type=float, default=0.2)
    parser.add_argument(
        "--list",
        action="store_true",
        help="list writable send nodes and exit",
    )
    parser.add_argument(
        "--check-node",
        action="store_true",
        help="strictly validate the selected send node type and access, then exit",
    )
    parser.add_argument(
        "--check-types",
        action="store_true",
        help="browse and strictly validate every <port>_Send node, then exit",
    )
    parser.add_argument(
        "--quality-node",
        help="with --check-types, validate this unavailable data node has Bad quality and no value",
    )
    parser.add_argument(
        "--expect-send-value",
        help="with --check-types or --check-node, require this current String value",
    )
    parser.add_argument(
        "--serial-monitor",
        help="optional receiving serial port used to verify actual bytes",
    )
    parser.add_argument("--serial-baud", type=int, default=28800)
    parser.add_argument("--serial-timeout", type=float, default=3.0)
    parser.add_argument(
        "--serial-reply-hex",
        default="",
        help="optional hex bytes sent back by the serial monitor after capture, for example 005050",
    )
    parser.add_argument(
        "--expect-status",
        choices=("received", "waiting", "failed", "any"),
        default="any",
        help="expected OPC status after write mode",
    )
    return parser.parse_args(list(argv))


def main(argv: Iterable[str]) -> int:
    args = parse_args(argv)
    command_hex = normalize_hex(args.hex)
    expected_frame = frame_with_checksum(command_hex)
    serial_reply = bytes.fromhex(normalize_hex(args.serial_reply_hex)) if args.serial_reply_hex else b""

    client = None
    try:
        client, ua = connect_client(args)
        namespace_index = client.get_namespace_index(args.namespace_uri)
        objects = client.get_objects_node()

        if args.list:
            nodes = browse_send_nodes(objects, ua, namespace_index)
            if not nodes:
                print("No <port>_Send nodes found.")
                return 1
            for name in nodes:
                print(name)
            return 0

        if args.check_types:
            nodes = browse_send_nodes(objects, ua, namespace_index)
            if not nodes:
                print("No <port>_Send nodes found.")
                return 1
            print(f"TYPE SCAN: found {len(nodes)} send node(s)")
            results = [
                inspect_send_node(
                    client,
                    ua,
                    namespace_index,
                    name,
                    args.expect_send_value,
                )
                for name in nodes
            ]
            metadata_ok = inspect_server_metadata(client, ua, namespace_index, nodes)
            quality_ok = (
                inspect_unavailable_data_quality(
                    client, ua, namespace_index, args.quality_node
                )
                if args.quality_node
                else True
            )
            passed = sum(results)
            print(f"TYPE SCAN RESULT: {passed}/{len(results)} passed")
            return 0 if all(results) and metadata_ok and quality_ok else 1

        send_name = args.node or (f"{opc_port_stem(args.port)}_Send" if args.port else None)
        if not send_name:
            nodes = browse_send_nodes(objects, ua, namespace_index)
            if not nodes:
                print("No <port>_Send nodes found. Pass --port or --node.")
                return 1
            send_name = nodes[0]
            print(f"Using first send node: {send_name}")

        if not send_name.endswith("_Send"):
            print(f"Invalid send node name: {send_name}")
            return 1

        port = send_name[: -len("_Send")]
        print(f"ENDPOINT: {args.endpoint}")
        print(f"NAMESPACE: {args.namespace_uri} => ns={namespace_index}")
        print(f"COMMAND BODY: {format_hex(bytes_from_hex(command_hex))}")
        print(f"EXPECTED SERIAL FRAME: {format_hex(expected_frame)}")

        if args.check_node:
            return (
                0
                if inspect_send_node(
                    client,
                    ua,
                    namespace_index,
                    send_name,
                    args.expect_send_value,
                )
                else 1
            )

        monitor_thread, monitor_result = start_serial_monitor(
            args.serial_monitor,
            args.serial_baud,
            args.serial_timeout,
            len(expected_frame),
            serial_reply,
        )

        if args.mode == "method":
            result = call_send_method(client, ua, namespace_index, port, command_hex)
            print(f"METHOD RESULT: {result}")
        else:
            node = node_for_send_name(client, ua, namespace_index, send_name)
            before = node.get_value()
            print(f"NODE BEFORE: {status_label(before)}")
            node.set_value(ua.Variant(command_hex, ua.VariantType.String))
            print(f"WRITE RESULT: OK ({send_name} = {command_hex})")
            expected_codes = {
                "received": {"1"},
                "waiting": {"2"},
                "failed": {"3"},
                "any": {"1", "2", "3"},
            }[args.expect_status]
            after = wait_for_status(
                node,
                args.status_timeout,
                args.poll_interval,
                expected_codes,
            )
            print(f"NODE AFTER: {status_label(after)}")

            if args.expect_status != "any":
                expected_code = {
                    "received": "1",
                    "waiting": "2",
                    "failed": "3",
                }[args.expect_status]
                if str(after) != expected_code:
                    print(
                        f"STATUS RESULT: MISMATCH expected {expected_code}, got {after!r}"
                    )
                    return 2
                print("STATUS RESULT: OK")

        if monitor_thread:
            monitor_thread.join(args.serial_timeout + 1.0)
            capture = monitor_result.get("capture", SerialCapture(b"", "monitor did not finish"))
            if not validate_serial_capture(capture, expected_frame):
                return 3

        return 0
    except Exception as exc:  # noqa: BLE001 - command-line diagnostics should show OPC errors
        print(f"ERROR: {exc}")
        return 1
    finally:
        if client is not None:
            try:
                client.disconnect()
            except Exception:
                pass


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
