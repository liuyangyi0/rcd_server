"""Explicit 58-only integration-label repair; never disables KYSEC or restarts.

Requires current authorization. Back up original labels before changing only
KID on the three newly provisioned shared lifecycle paths; preserve lock inode.
"""
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import stat
import subprocess
import time


def repair(backup):
    if os.geteuid() != 0:
        raise ValueError('authorized root required')
    addresses = json.loads(subprocess.check_output(['ip', '-j', 'address']))
    if not any(a.get('local') == '192.168.10.58' for link in addresses for a in link.get('addr_info', [])):
        raise ValueError('this field repair is scoped to host 58')
    root = Path('/opt/rcd/bin')
    reference = os.getxattr(root / 'RcdAgent', 'security.ksip', follow_symlinks=False)
    if reference != b'none:none:unknown:999':
        raise ValueError('Agent label changed; inspect instead of guessing')
    names = ['.rcd-host-release.lock', '.rcd-host-release-active.json', '.rcd-host-control']
    descriptors, originals = [], []
    try:
        for name in names:
            path = root / name
            fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
            descriptors.append(fd)
            info = os.fstat(fd)
            directory = name == '.rcd-host-control'
            if not (stat.S_ISDIR(info.st_mode) if directory else stat.S_ISREG(info.st_mode) and info.st_nlink == 1):
                raise ValueError('unsafe target: ' + name)
            if name.endswith('.lock'):
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            content = b'' if directory else os.read(fd, 65537)
            if len(content) > 65536:
                raise ValueError('unexpected target size')
            if name.endswith('.json') and json.loads(content)['state'] not in ('succeeded', 'recovered', 'failed_before_install'):
                raise ValueError('unfinished release; reconcile original operation')
            if directory and list(path.iterdir()):
                raise ValueError('control state is no longer empty; inspect current operation')
            label = os.getxattr(fd, 'security.ksip')
            expected_old = b'none:none:trusted:10000' if directory else b'none:none:unknown:9999'
            if label != expected_old:
                raise ValueError('label changed; do not replay repair: ' + name)
            originals.append({'path': str(path), 'label': label.decode(), 'inode': info.st_ino,
                              'uid': info.st_uid, 'gid': info.st_gid, 'mode': info.st_mode,
                              'sha256': None if directory else hashlib.sha256(content).hexdigest()})
        with backup.open('x', encoding='utf-8') as stream:
            os.fchmod(stream.fileno(), 0o600)
            json.dump({'time': time.time(), 'reference': reference.decode(), 'targets': originals}, stream, indent=2)
            stream.flush(); os.fsync(stream.fileno())
        for fd, original in zip(descriptors, originals):
            expected = ':'.join(original['label'].split(':')[:3] + ['999']).encode()
            os.setxattr(fd, 'security.ksip', expected)
            info = os.fstat(fd)
            assert os.getxattr(fd, 'security.ksip') == expected
            assert (info.st_ino, info.st_uid, info.st_gid, info.st_mode) == tuple(original[k] for k in ('inode', 'uid', 'gid', 'mode'))
            if original['sha256'] is not None:
                os.lseek(fd, 0, os.SEEK_SET)
                assert hashlib.sha256(os.read(fd, 65537)).hexdigest() == original['sha256']
        print(json.dumps({'repaired': [x['path'] for x in originals], 'backup': str(backup), 'restarted': False}))
    finally:
        for fd in reversed(descriptors):
            os.close(fd)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--confirm-scoped-label-repair', action='store_true', required=True)
    parser.add_argument('--backup', type=Path, required=True)
    args = parser.parse_args()
    repair(args.backup)
