#!/usr/bin/env python3
"""Explicit, one-host maintenance helper. Only updates disabled sync46 service.

Not a replacement for the platform publisher. Run with sudo after isolated
acceptance. Optionally migrates the explicitly supplied receive point table;
never changes config.toml, service definitions, or other apps.
"""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import pwd
import re
import shutil
import socket
import subprocess
import tempfile
import time

ENTRY = Path('/opt/rcd/bin/rcd_communicate')
DATA = Path('/var/lib/rcd/communicate/sync46')
SERVICE = 'rcd-communicate.service'


def sha(path):
    with open(path, 'rb') as source:
        return hashlib.sha256(source.read()).hexdigest()


def control(*args):
    uid = pwd.getpwnam('lenovo').pw_uid
    return subprocess.check_output(['runuser', '-u', 'lenovo', '--', 'env',
        'XDG_RUNTIME_DIR=/run/user/' + str(uid), 'systemctl', '--user', *args], text=True).strip()


def snapshot():
    values = control('show', SERVICE, '-p', 'MainPID', '-p', 'ActiveState', '-p', 'NRestarts')
    return dict(line.split('=', 1) for line in values.splitlines())


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--artifact', type=Path, required=True)
    parser.add_argument('--sha256', required=True)
    parser.add_argument('--evidence', type=Path, required=True)
    parser.add_argument('--points', type=Path)
    parser.add_argument('--points-sha256')
    parser.add_argument('--deploy', action='store_true', required=True)
    args = parser.parse_args()
    assert os.geteuid() == 0, 'sudo required'
    assert re.fullmatch('[a-f0-9]{64}', args.sha256)
    assert args.artifact.is_file() and sha(args.artifact) == args.sha256, 'artifact mismatch'
    assert bool(args.points) == bool(args.points_sha256), 'point table and digest required together'
    if args.points:
        assert re.fullmatch('[a-f0-9]{64}', args.points_sha256)
        assert args.points.is_file() and not args.points.is_symlink()
        assert sha(args.points) == args.points_sha256, 'point table mismatch'
    # Verify host by binding its assigned IP, not by a potentially shared name.
    with socket.socket() as sock:
        sock.bind(('192.168.10.46', 0))
    assert ENTRY.is_file() and not ENTRY.is_symlink()
    fd = os.open(ENTRY.parent / '.rcd-host-release.lock', os.O_RDWR | os.O_NOFOLLOW)
    with os.fdopen(fd, 'r+') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        marker = ENTRY.parent / '.rcd-host-release-active.json'
        if marker.exists():
            assert not marker.is_symlink()
            assert json.loads(marker.read_text()).get('state') == 'succeeded', 'unresolved platform maintenance marker; stop for reconciliation'
        config = DATA / 'config/config.toml'
        text = config.read_text()
        assert not re.search(r'^\s*(?:enabled|transmit_enabled)\s*=\s*true\b', text, re.M)
        assert re.search(r'^instance_id\s*=\s*"communicate-46"', text, re.M)
        assert re.search(r'^port\s*=\s*4842\b', text, re.M)
        assert '/dev/rcd-communicate-unassigned' in text
        assert '/opt/rcd/bin/rcd_communicate run ' in control('show', SERVICE, '-p', 'ExecStart')
        points = config.parent / 'points.csv'
        assert not config.is_symlink() and not points.is_symlink()
        if args.points:
            assert re.search(r'^points_file\s*=\s*"points.csv"\s*$', text, re.M)
            # Validate the coordinated set before stopping the existing process.
            with tempfile.TemporaryDirectory(prefix='rcd-points-check-') as validation:
                candidate = Path(validation)
                shutil.copy2(config, candidate / 'config.toml')
                shutil.copy2(args.points, candidate / 'points.csv')
                subprocess.run([str(args.artifact), 'check-config', '--config', str(candidate / 'config.toml')], check=True)
        else:
            subprocess.run([str(args.artifact), 'check-config', '--config', str(config)], check=True)
        before = snapshot()
        assert before['ActiveState'] == 'active' and int(before['MainPID']) > 0
        config_hashes = {p.name: sha(p) for p in (DATA / 'config').iterdir() if p.is_file()}
        args.evidence.mkdir(mode=0o700, parents=False, exist_ok=False)
        shutil.copy2(ENTRY, args.evidence / 'rcd_communicate.previous')
        for name in ('config', 'state', 'pki'):
            shutil.copytree(DATA / name, args.evidence / name)
        record = {'before': before, 'previous_sha256': sha(ENTRY), 'target_sha256': args.sha256,
                  'config_sha256': config_hashes, 'started_at': time.strftime('%Y-%m-%dT%H:%M:%S%z'),
                  'state': 'prepared', 'serial_enabled': False}
        expected_hashes = dict(config_hashes)
        if args.points:
            expected_hashes['points.csv'] = args.points_sha256
            record['target_points_sha256'] = args.points_sha256
        def save():
            target = args.evidence / 'deployment.json'
            with open(target, 'w') as out:
                json.dump(record, out, indent=2); out.flush(); os.fsync(out.fileno())
        save()
        temporary = None
        temporary_points = None
        try:
            with tempfile.NamedTemporaryFile(dir=ENTRY.parent, prefix='.communicate-', delete=False) as out:
                temporary = Path(out.name)
                with open(args.artifact, 'rb') as source: shutil.copyfileobj(source, out)
                out.flush(); os.fchmod(out.fileno(), 0o755); os.fsync(out.fileno())
            assert sha(temporary) == args.sha256
            if args.points:
                original_stat = points.stat()
                with tempfile.NamedTemporaryFile(dir=points.parent, prefix='.points-', delete=False) as out:
                    temporary_points = Path(out.name)
                    with open(args.points, 'rb') as source: shutil.copyfileobj(source, out)
                    out.flush(); os.fchmod(out.fileno(), original_stat.st_mode & 0o777)
                    os.fchown(out.fileno(), original_stat.st_uid, original_stat.st_gid)
                    os.fsync(out.fileno())
                assert sha(temporary_points) == args.points_sha256
            control('stop', SERVICE)
            assert int(snapshot()['MainPID']) == 0, 'process did not stop'
            assert all(sha(DATA / 'config' / name) == value for name, value in config_hashes.items())
            if temporary_points is not None:
                os.replace(temporary_points, points); temporary_points = None
                record['state'] = 'points_replaced'; save()
            os.replace(temporary, ENTRY); temporary = None
            for directory in (ENTRY.parent, points.parent):
                directory_fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
                try: os.fsync(directory_fd)
                finally: os.close(directory_fd)
            record['state'] = 'replaced'; save()
            control('start', SERVICE)
            for _ in range(90):
                current = snapshot()
                pid = int(current['MainPID'])
                if current['ActiveState'] == 'active' and pid and sha(Path('/proc') / str(pid) / 'exe') == args.sha256:
                    # Inspect the owning listener without opening then dropping
                    # an incomplete OPC handshake (which logs a false alarm).
                    listeners = subprocess.check_output(['ss', '-H', '-ltnp', 'sport = :4842'], text=True)
                    if 'pid=' + str(pid) + ',' in listeners: break
                time.sleep(1)
            else: raise RuntimeError('new service not ready; preserve evidence for explicit rollback')
            assert all(sha(DATA / 'config' / name) == value for name, value in expected_hashes.items())
            subprocess.run([str(ENTRY), 'check-config', '--config', str(config)], check=True)
            record.update(state='started', after=snapshot(), finished_at=time.strftime('%Y-%m-%dT%H:%M:%S%z'))
            save(); print(json.dumps(record))
        except BaseException:
            record['state'] = 'needs_review'; save()
            raise
        finally:
            if temporary is not None: temporary.unlink(missing_ok=True)
            if temporary_points is not None: temporary_points.unlink(missing_ok=True)


if __name__ == '__main__':
    main()
