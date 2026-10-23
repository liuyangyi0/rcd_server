#!/usr/bin/env python3
"""Read-only Mender evaluation preflight. Does not install or start anything."""
import argparse
import json
from pathlib import Path
import platform
import shutil
import socket
import subprocess

GIB = 1024 ** 3


def assess(facts):
    failures = []
    if facts['system'] != 'Linux' or facts['machine'] not in ('x86_64', 'aarch64'):
        failures.append('supported Linux architecture required')
    # Mongo's x86 builds require AVX; do not infer it from the CPU vendor.
    if facts['machine'] == 'x86_64' and 'avx' not in facts['cpu_flags']:
        failures.append('MongoDB requires AVX on x86_64')
    if facts['cpu_count'] < 2:
        failures.append('at least 2 logical CPUs required')
    # Preserve headroom for the existing OPC processes on a shared host.
    if facts['memory_available_bytes'] < 16 * GIB:
        failures.append('at least 16 GiB currently available memory required')
    if facts['disk_free_bytes'] < 50 * GIB:
        failures.append('at least 50 GiB free storage required')
    if facts['occupied_ports']:
        failures.append('requested evaluation ports are already occupied')
    return {'schema_version': 1, 'resource_ready': not failures,
            'failures': failures, 'facts': facts,
            'scope': 'read-only; no service, firewall, configuration or credential changes'}


def collect(storage, bind_address, ports):
    if platform.system() != 'Linux':
        raise RuntimeError('preflight must run on the target Linux host')
    if not storage.is_absolute() or not storage.is_dir():
        raise ValueError('existing absolute storage directory required')
    memory = {}
    for line in Path('/proc/meminfo').read_text().splitlines():
        key, value = line.split(':', 1)
        memory[key] = int(value.split()[0]) * 1024
    cpus = Path('/proc/cpuinfo').read_text()
    flag_sets = [set(line.split(':', 1)[1].split()) for line in cpus.splitlines()
                 if line.startswith('flags') or line.startswith('Features')]
    flags = set.intersection(*flag_sets) if flag_sets else set()
    occupied = []
    for port in ports:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
            try:
                listener.bind((bind_address, port))
            except OSError:
                occupied.append(port)
    tools = {}
    for name, command in [('docker', ['docker', '--version']),
                          ('compose', ['docker', 'compose', 'version', '--short'])]:
        try:
            tools[name] = subprocess.check_output(command, text=True, timeout=10,
                                                  stderr=subprocess.STDOUT).strip()
        except (OSError, subprocess.SubprocessError):
            tools[name] = None
    return {'system': platform.system(), 'machine': platform.machine(),
            'cpu_count': len(__import__('os').sched_getaffinity(0)), 'cpu_flags': sorted(flags),
            'memory_total_bytes': memory['MemTotal'],
            'memory_available_bytes': memory['MemAvailable'],
            'disk_free_bytes': shutil.disk_usage(storage).free,
            'storage': str(storage), 'bind_address': bind_address,
            'requested_ports': ports, 'occupied_ports': occupied, 'tools': tools}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--storage', type=Path, required=True)
    parser.add_argument('--bind-address', required=True)
    parser.add_argument('--ports', type=int, nargs='+', default=[8443, 9443])
    args = parser.parse_args()
    if any(not 1024 <= port <= 65535 for port in args.ports):
        parser.error('evaluation ports must be unprivileged valid TCP ports')
    result = assess(collect(args.storage, args.bind_address, args.ports))
    print(json.dumps(result, ensure_ascii=False, indent=2))
    return 0 if result['resource_ready'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
