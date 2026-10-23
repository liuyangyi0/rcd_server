#!/usr/bin/env python3
"""One authorized native Semaphore upload/upgrade; no automatic replay."""
import argparse
import getpass
import json
import os
from pathlib import Path
import subprocess


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--root', type=Path, required=True)
    parser.add_argument('--browser-script', type=Path, required=True)
    parser.add_argument('--version', required=True)
    parser.add_argument('--mode', choices=['upload', 'upgrade'], required=True)
    parser.add_argument('--confirm-paused', action='store_true', required=True)
    args = parser.parse_args()
    assert '192.168.10.46' in subprocess.check_output(['hostname', '-I'], text=True).split()
    root = args.root.resolve(strict=True)
    output = root / 'deployment-browser' / args.mode
    assert not output.exists(), 'Inspect the existing receipt; do not replay'
    target = '46' if args.mode == 'upgrade' else str(root / 'release-manager' / ('manager-' + args.version + '.rcd.tar.gz'))
    secret = getpass.getpass('Semaphore password: ')
    env = os.environ.copy()
    env['RCD_PLAYWRIGHT_MODULE'] = '/home/lenovo/code/rcd_manager_test/browser-tools/node_modules/playwright'
    result = subprocess.run(['/home/lenovo/code/rcd_manager_test/tools/node-v24.14.0-linux-x64/bin/node',
        str(args.browser_script.resolve(strict=True)), str(output), args.mode, 'manager', args.version, target, '--confirm-paused'],
        input=json.dumps({'platform': secret}), text=True, env=env)
    raise SystemExit(result.returncode)


if __name__ == '__main__':
    main()
