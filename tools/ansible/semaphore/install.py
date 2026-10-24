#!/usr/bin/env python3
"""Explicit fresh Semaphore install on the approved Linux controller, not an updater.

No OPC process control. Refuses existing platform paths. Run with root only after
checking the official pinned archive checksum. Secrets are generated on target.
"""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import pwd
import secrets
import shutil
import socket
import subprocess


def run(*args, **kwargs):
    subprocess.run(args, check=True, **kwargs)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--download-dir', type=Path, required=True)
    parser.add_argument('--ansible-tools', type=Path, required=True)
    parser.add_argument('--playbooks', type=Path, required=True)
    parser.add_argument('--host', required=True)
    parser.add_argument('--finish-existing', action='store_true', help='finish this fresh install after pre-start failure; never upgrade an existing service')
    args = parser.parse_args()
    if os.geteuid() != 0 or args.host not in subprocess.check_output(['hostname', '-I'], text=True).split():
        raise RuntimeError('root and explicit local host required')
    version = '2.19.12'
    archive = args.download_dir / ('semaphore_community_' + version + '_linux_amd64.tar.gz')
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    checks = (args.download_dir / 'checksums.txt').read_text().splitlines()
    if not any(line.split() == [digest, archive.name] for line in checks):
        raise RuntimeError('official archive checksum mismatch')
    reported = subprocess.check_output([str(args.download_dir / 'semaphore'), 'version'], text=True)
    if version not in reported:
        raise RuntimeError('unexpected Semaphore version')
    if args.finish_existing:
        if Path('/opt/semaphore/install-record.json').exists() or Path('/etc/systemd/system/semaphore.service').exists():
            raise RuntimeError('already installed; finish-existing is not an updater')
        if hashlib.sha256(Path('/opt/semaphore/bin/semaphore').read_bytes()).digest() != hashlib.sha256((args.download_dir / 'semaphore').read_bytes()).digest():
            raise RuntimeError('partial install binary differs from verified download')
        os.chmod('/opt/semaphore/bin', 0o755)
        finish_install(args, version, digest)
        return
    for path in ('/opt/semaphore', '/etc/semaphore', '/var/lib/semaphore', '/var/cache/semaphore',
                 '/etc/systemd/system/semaphore.service'):
        if Path(path).exists():
            raise RuntimeError('fresh install refuses existing path: ' + path)
    with socket.socket() as probe:
        probe.bind((args.host, 3000))
    try:
        pwd.getpwnam('semaphore')
    except KeyError:
        run('useradd', '--system', '--home-dir', '/var/lib/semaphore', '--shell', '/usr/sbin/nologin', 'semaphore')
    else:
        raise RuntimeError('existing semaphore user needs manual review')
    account = pwd.getpwnam('semaphore')
    for path in ('/opt/semaphore/bin', '/etc/semaphore', '/var/lib/semaphore', '/var/cache/semaphore'):
        Path(path).mkdir(parents=True, mode=0o750)
    for path in ('/var/lib/semaphore', '/var/cache/semaphore'):
        os.chown(path, account.pw_uid, account.pw_gid)
        os.chmod(path, 0o700)
    shutil.copy2(args.download_dir / 'semaphore', '/opt/semaphore/bin/semaphore')
    os.chmod('/opt/semaphore/bin', 0o755)
    os.chmod('/opt/semaphore/bin/semaphore', 0o755)
    python = args.ansible_tools / 'python/cpython-3.13.15-linux-x86_64-gnu'
    shutil.copytree(python, '/opt/semaphore/python', symlinks=True)
    # Create a fresh venv at its final path; don't copy absolute shebangs from the
    # earlier isolated test environment or depend on a user's private home.
    run(str(args.ansible_tools / 'uv'), 'venv', '/opt/semaphore/venv',
        '--python', '/opt/semaphore/python/bin/python3.13')
    shutil.copy2(args.playbooks / 'tools/ansible/requirements.lock', '/opt/semaphore/requirements.lock')
    run(str(args.ansible_tools / 'uv'), 'pip', 'sync', '--python', '/opt/semaphore/venv/bin/python',
        '--cache-dir', str(args.ansible_tools / 'cache'), '/opt/semaphore/requirements.lock')
    shutil.copytree(args.playbooks / 'tools', '/opt/semaphore/playbooks/tools',
                    ignore=shutil.ignore_patterns('__pycache__', '*.pyc'))
    config = {'dialect': 'sqlite', 'sqlite': {'host': '/var/lib/semaphore/semaphore.sqlite', 'name': 'semaphore'},
              'interface': args.host, 'port': ':3000', 'web_host': 'http://' + args.host + ':3000',
              'tmp_path': '/var/cache/semaphore', 'max_parallel_tasks': 1,
              'cookie_hash': base64.b64encode(secrets.token_bytes(32)).decode(),
              'cookie_encryption': base64.b64encode(secrets.token_bytes(32)).decode(),
              'access_key_encryption': base64.b64encode(secrets.token_bytes(32)).decode(),
              'env_vars': {'ANSIBLE_HOST_KEY_CHECKING': 'True', 'ANSIBLE_SSH_HOST_KEY_CHECKING': 'True'}}
    target = Path('/etc/semaphore/config.json')
    target.write_text(json.dumps(config, indent=2))
    os.chown(target, 0, account.pw_gid)
    target.chmod(0o640)
    os.chown('/etc/semaphore', 0, account.pw_gid)
    password = secrets.token_urlsafe(20)
    secret = Path('/etc/semaphore/initial-admin-password')
    secret.write_text(password + '\n')
    secret.chmod(0o600)
    finish_install(args, version, digest)


def finish_install(args, version, digest):
    account = pwd.getpwnam('semaphore')
    target = Path('/etc/semaphore/config.json')
    config = json.loads(target.read_text())
    # v2.19.12's SQLite driver opens sqlite.host (not sqlite.name).
    if not config['sqlite'].get('host') and config['sqlite'].get('name') == '/var/lib/semaphore/semaphore.sqlite':
        config['sqlite'] = {'host': '/var/lib/semaphore/semaphore.sqlite', 'name': 'semaphore'}
        target.write_text(json.dumps(config, indent=2))
    shutil.copy2(args.playbooks / 'tools/ansible/requirements.lock', '/opt/semaphore/requirements.lock')
    run(str(args.ansible_tools / 'uv'), 'pip', 'sync', '--python', '/opt/semaphore/venv/bin/python',
        '--cache-dir', str(args.ansible_tools / 'cache'), '/opt/semaphore/requirements.lock')
    password = Path('/etc/semaphore/initial-admin-password').read_text().strip()
    # Never print the administrator password or persist it in a unit/environment.
    run('runuser', '-u', 'semaphore', '--', '/opt/semaphore/bin/semaphore', 'migrate',
        '--config', str(target))
    run('runuser', '-u', 'semaphore', '--', '/opt/semaphore/bin/semaphore', 'user', 'add',
        '--config', str(target), '--login', 'admin', '--name', 'RCD Administrator',
        '--email', 'admin@localhost', '--password', password, '--admin')
    known_hosts = Path('/var/lib/semaphore/.ssh/known_hosts')
    known_hosts.parent.mkdir(mode=0o700)
    # Public host keys are copied from previously trusted entries, not accepted
    # blindly from ssh-keyscan. The controller's own public key is authoritative.
    keys = []
    for host in ('192.168.10.30', '192.168.10.58'):
        result = subprocess.run(['ssh-keygen', '-F', host, '-f', '/home/lenovo/.ssh/known_hosts'],
                                text=True, capture_output=True, check=True)
        keys.extend(line for line in result.stdout.splitlines() if not line.startswith('#'))
    keys.append(args.host + ' ' + Path('/etc/ssh/ssh_host_ed25519_key.pub').read_text().strip())
    known_hosts.write_text('\n'.join(keys) + '\n')
    for path in (known_hosts.parent, known_hosts):
        os.chown(path, account.pw_uid, account.pw_gid)
    known_hosts.chmod(0o600)
    unit = Path(__file__).with_name('semaphore.service')
    shutil.copy2(unit, '/etc/systemd/system/semaphore.service')
    Path('/etc/systemd/system/semaphore.service').chmod(0o644)
    record = {'version': version, 'archive_sha256': digest, 'host': args.host,
              'url': config['web_host'], 'database': config['sqlite']['host'],
              'service': 'semaphore.service', 'ansible_version': '2.19.13',
              'opc_services_changed': False}
    Path('/opt/semaphore/install-record.json').write_text(json.dumps(record, indent=2))
    run('systemctl', 'daemon-reload')
    run('systemctl', 'enable', '--now', 'semaphore.service')
    print(json.dumps(record))


if __name__ == '__main__':
    main()
