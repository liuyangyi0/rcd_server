#!/usr/bin/env python3
"""Explicit 46-only platform update. No RCD unit or device command is executed.

An idle queue and a consistent database/config/binary/package backup are required.
Automatic rollback is confined to this initial installation window, before the
platform is handed back to users; it restores the matching platform DB as well.
"""
import argparse
from contextlib import closing
import fcntl
import hashlib
import json
import os
from pathlib import Path
import pwd
import shutil
import sqlite3
import subprocess
import tarfile
import time
import urllib.request


def digest(path):
    h = hashlib.sha256()
    with Path(path).open('rb') as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b''): h.update(chunk)
    return h.hexdigest()


def atomic_copy(src, dst, mode):
    dst = Path(dst)
    temp = dst.with_name(dst.name + '.rcd-new')
    if temp.exists(): raise RuntimeError('unresolved staging file: ' + str(temp))
    shutil.copyfile(src, temp)
    temp.chmod(mode)
    with temp.open('rb') as stream: os.fsync(stream.fileno())
    os.replace(temp, dst)


def idle(db):
    with closing(sqlite3.connect(db.as_uri() + '?mode=ro', uri=True)) as conn:
        if conn.execute("select count(*) from task where status in ('waiting','starting','running')").fetchone()[0]:
            raise RuntimeError('native tasks are active; no platform restart performed')
        if conn.execute("select count(*) from sqlite_master where name='rcd_operation_lock'").fetchone()[0]:
            if conn.execute('select count(*) from rcd_operation_lock').fetchone()[0]:
                raise RuntimeError('unresolved release operation; inspect it before platform update')
        if conn.execute("select count(*) from sqlite_master where name='rcd_host_setup_active'").fetchone()[0]:
            if conn.execute('select count(*) from rcd_host_setup_active').fetchone()[0]:
                raise RuntimeError('unresolved host setup; inspect or safely cancel it before platform update')


def restore_database(db, backup, migrated, uid, gid):
    # Backup failure before first start must not discard the sole live copy.
    if not migrated:
        return
    saved = backup/'semaphore.sqlite'
    with closing(sqlite3.connect(saved.as_uri()+'?mode=ro',uri=True)) as check:
        if check.execute('pragma quick_check').fetchall()!=[('ok',)]:
            raise RuntimeError('matching platform database backup is invalid')
    for suffix in ('','-wal','-shm'):
        path=Path(str(db)+suffix)
        if path.exists(): shutil.move(path, backup/('failed-database'+suffix))
    shutil.copy2(saved,db)
    os.chown(db,uid,gid)


def install(binary, expected, source, record):
    if os.geteuid() != 0 or '192.168.10.46' not in subprocess.check_output(['hostname','-I'], text=True).split():
        raise RuntimeError('run as root on explicitly authorized 46 only')
    binary, source, record = binary.resolve(strict=True), source.resolve(strict=True), record.resolve(strict=True)
    if digest(binary) != expected: raise RuntimeError('binary hash differs')
    upstream = json.loads(record.read_text())['upstream']
    if upstream['commit'] != 'f8cefe62da4d40e536323e3dd936eb2e04e82b1a': raise RuntimeError('unexpected upstream')
    config = Path('/etc/semaphore/config.json')
    settings = json.loads(config.read_text())
    db = Path(settings['sqlite']['host'])
    if db != Path('/var/lib/semaphore/semaphore.sqlite'): raise RuntimeError('review unexpected database binding')
    account = pwd.getpwnam('semaphore')
    current = Path('/opt/semaphore/bin/semaphore')
    drop = Path('/etc/systemd/system/semaphore.service.d/95-rcd-native.conf')
    with Path('/var/lib/semaphore/.platform-update.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        idle(db)
        backup = Path('/var/lib/semaphore/backups') / ('native-platform-' + str(time.time_ns()))
        backup.mkdir(parents=True, mode=0o700)
        shutil.copy2(current, backup/'semaphore')
        shutil.copy2(config, backup/'config.json')
        if drop.exists(): shutil.copy2(drop, backup/'drop-in.conf')
        snapshot = Path('/opt/semaphore/rcd-platform') / (expected[:16] + '-' + backup.name)
        if snapshot.exists(): raise RuntimeError('snapshot exists; inspect the original update, do not overwrite')
        snapshot.mkdir(parents=True, mode=0o755)
        for name in ('release','ansible'):
            shutil.copytree(source/'tools'/name, snapshot/'tools'/name,
                            ignore=shutil.ignore_patterns('__pycache__','*.pyc'))
        shutil.copy2(record, snapshot/'rcd-source-record.json')
        files = {str(p.relative_to(snapshot)):digest(p) for p in snapshot.rglob('*') if p.is_file()}
        (snapshot/'manifest.json').write_text(json.dumps(files, indent=2))
        subprocess.run(['systemctl','stop','semaphore.service'], check=True)
        migrated = False
        try:
            idle(db)
            with closing(sqlite3.connect(db.as_uri()+'?mode=ro',uri=True)) as src, closing(sqlite3.connect(backup/'semaphore.sqlite')) as dst:
                src.backup(dst)
                if dst.execute('pragma quick_check').fetchall()!=[('ok',)]: raise RuntimeError('backup integrity failed')
            with tarfile.open(backup/'platform-files.tar.gz','w:gz') as tar:
                for path in ('/opt/semaphore/playbooks','/etc/semaphore','/var/lib/semaphore/rcd-packages'):
                    if Path(path).exists(): tar.add(path, arcname=path.lstrip('/'))
            packages = Path('/var/lib/semaphore/rcd-packages')
            packages.mkdir(mode=0o700, exist_ok=True)
            os.chown(packages, account.pw_uid, account.pw_gid)
            drop.parent.mkdir(parents=True, exist_ok=True)
            drop.write_text('[Service]\nEnvironment=SEMAPHORE_RCD_PACKAGES_PATH=/var/lib/semaphore/rcd-packages\n')
            atomic_copy(binary, current, 0o755)
            subprocess.run(['systemctl','daemon-reload'], check=True)
            migrated = True
            subprocess.run(['systemctl','start','semaphore.service'], check=True)
            for attempt in range(45):
                try:
                    with urllib.request.urlopen(settings['web_host'].rstrip('/')+'/api/ping', timeout=3) as response:
                        if response.read()!=b'pong': raise RuntimeError('platform ping failed')
                    with closing(sqlite3.connect(db.as_uri()+'?mode=ro',uri=True)) as check:
                        if check.execute("select count(*) from sqlite_master where name in ('rcd_package','rcd_device','rcd_operation')").fetchone()[0]!=3:
                            raise RuntimeError('RCD migrations not ready')
                    break
                except Exception:
                    if attempt == 44: raise
                    time.sleep(1)
            if digest(config)!=digest(backup/'config.json'): raise RuntimeError('configuration unexpectedly changed')
        except BaseException:
            subprocess.run(['systemctl','stop','semaphore.service'], check=True)
            # Retain the failed migrated DB and sidecars as evidence, never mix
            # them with the old schema. No live business database is touched.
            restore_database(db,backup,migrated,account.pw_uid,account.pw_gid)
            if digest(current)!=digest(backup/'semaphore'):
                atomic_copy(backup/'semaphore',current,0o755)
            if (backup/'drop-in.conf').exists(): shutil.copy2(backup/'drop-in.conf',drop)
            elif drop.exists(): drop.unlink()
            subprocess.run(['systemctl','daemon-reload'],check=True)
            subprocess.run(['systemctl','start','semaphore.service'],check=True)
            raise
        result={'backup':str(backup),'binary_sha256':expected,'repository':str(snapshot),
                'upstream':upstream,'config_sha256':digest(config),'business_services_changed':False}
        (backup/'result.json').write_text(json.dumps(result,indent=2))
        print(json.dumps(result))


if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--binary',type=Path,required=True)
    p.add_argument('--sha256',required=True)
    p.add_argument('--source',type=Path,required=True)
    p.add_argument('--source-record',type=Path,required=True)
    a=p.parse_args()
    install(a.binary,a.sha256,a.source,a.source_record)
