#!/usr/bin/env python3
"""Run a fresh loopback-only Semaphore for remote browser acceptance.

Never reads the live Semaphore config/database or registers inventories/tasks.
Terminate with Ctrl-C; the child is reaped. Existing directories are rejected.
"""
import argparse
import base64
import json
import os
from pathlib import Path
import secrets
import signal
import socket
import subprocess


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--directory', type=Path, required=True)
    args = parser.parse_args()
    if not args.binary.is_absolute() or not args.directory.is_absolute():
        raise ValueError('absolute isolated paths required')
    args.binary = args.binary.resolve(strict=True)
    args.directory = args.directory.parent.resolve(strict=True) / args.directory.name
    if (not str(args.directory).startswith(('/home/lenovo/code/rcd_semaphore_', '/home/lenovo/code/rcd_unified_')) or
            '..' in args.directory.parts or not args.binary.is_file()):
        raise ValueError('only the isolated 46 workspace is permitted')
    args.directory.mkdir(mode=0o700, parents=False, exist_ok=False)
    root = args.directory
    with socket.socket() as probe:
        probe.bind(('127.0.0.1', 0))
        port = probe.getsockname()[1]
    url = 'http://127.0.0.1:' + str(port)
    config = {'dialect': 'sqlite', 'sqlite': {'host': str(root / 'semaphore.sqlite'), 'name': 'semaphore'},
              'interface': '127.0.0.1', 'port': ':' + str(port), 'web_host': url,
              'tmp_path': str(root / 'tmp'), 'max_parallel_tasks': 1}
    for name in ('cookie_hash', 'cookie_encryption', 'access_key_encryption'):
        config[name] = base64.b64encode(secrets.token_bytes(32)).decode()
    config_path = root / 'config.json'
    config_path.write_text(json.dumps(config))
    config_path.chmod(0o600)
    password = secrets.token_urlsafe(24)
    login = root / 'browser-credentials.json'
    login.write_text(json.dumps({'url': url, 'auth': 'rcd-test', 'password': password}))
    login.chmod(0o600)
    env = dict(os.environ, SEMAPHORE_RCD_PACKAGES_PATH=str(root / 'packages'))
    # Native CLI performs native migrations and password hashing.
    with (root / 'setup.log').open('w') as output:
        subprocess.run([str(args.binary), 'user', 'add', '--config', str(config_path),
                        '--login', 'rcd-test', '--name', 'RCD Test', '--email', 'rcd-test@example.invalid',
                        '--admin', '--password', password], env=env, stdout=output, stderr=subprocess.STDOUT, check=True)
    print('Isolated Semaphore: ' + url, flush=True)
    process = None
    def terminate(_signum, _frame):
        raise KeyboardInterrupt()
    signal.signal(signal.SIGTERM, terminate)
    try:
        with (root / 'server.log').open('w') as output:
            process = subprocess.Popen([str(args.binary), 'server', '--config', str(config_path)],
                                       env=env, stdout=output, stderr=subprocess.STDOUT)
            process.wait()
    finally:
        if process and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)


if __name__ == '__main__':
    try:
        main()
    except KeyboardInterrupt:
        pass
