#!/usr/bin/env python3
"""Run on 46 only: isolated loopback OPC fixture, never opens serial ports."""
import argparse
import json
from pathlib import Path
import subprocess
from run_opc_checks import free_port, wait_ready, run_probe


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', required=True, type=Path)
    parser.add_argument('--fixture', required=True, type=Path)
    parser.add_argument('--probe', required=True, type=Path)
    parser.add_argument('--scratch', required=True, type=Path)
    args = parser.parse_args()
    source, fixture, probe, scratch = (p.resolve(strict=True) for p in
        (args.source, args.fixture, args.probe, args.scratch))
    if any(scratch.iterdir()) or scratch == source or source in scratch.parents:
        raise RuntimeError('scratch must be empty and outside source')
    reports = []
    for security in ('none', 'secure'):
        root = scratch / security
        root.mkdir()
        port = free_port()
        template = (source / 'examples/unit1-receiver.toml').read_text()
        if 'enabled = true' in template or 'bind_ip = "127.0.0.1"' not in template:
            raise RuntimeError('unsafe fixture template')
        (root / 'config.toml').write_text(template.replace('port = 14842', f'port = {port}'))
        (root / 'unit1-receiver.csv').write_bytes((source / 'examples/unit1-receiver.csv').read_bytes())
        with (root / 'server.log').open('w') as log:
            process = subprocess.Popen([str(fixture), str(root)], cwd=root, stdout=log, stderr=subprocess.STDOUT)
            try:
                wait_ready(process, port)
                reports.append(run_probe(probe, root, port, security, 'report'))
                if process.wait(timeout=55) != 0:
                    raise RuntimeError('fixture shutdown failed')
            finally:
                if process.poll() is None:
                    process.terminate()
                    process.wait(timeout=10)
    report = {'passed': True, 'hardware_used': False, 'reports': reports}
    (scratch / 'report.json').write_text(json.dumps(report, indent=2)+'\n')
    print(json.dumps(report))


if __name__ == '__main__':
    main()
