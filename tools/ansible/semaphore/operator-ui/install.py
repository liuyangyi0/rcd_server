"""Update ONLY the approved Semaphore UI and its read-only inspector on 46.

Requires idle task queue, preserves config/database, never touches RCD units.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import sqlite3
import subprocess
import sys
import time
import urllib.request

parser=argparse.ArgumentParser()
parser.add_argument('action',choices=['inspector','ui'])
parser.add_argument('--sha256')
args=parser.parse_args()
if args.action == 'ui':
    parser.error('The custom UI was retired. Use official Semaphore and native Views; see ../README.md.')
root=Path('/home/lenovo/code/rcd_semaphore_ui_20260930')
sys.path.insert(0,'/opt/semaphore/playbooks/tools/ansible/semaphore')
from connect_release import connection
assert os.geteuid()==0
assert '192.168.10.46' in subprocess.check_output(['hostname','-I'],text=True).split()
api=connection()
assert not any(t['status'] in ('waiting','starting','running') for t in api('/project/1/tasks?count=200')),'platform tasks are active'
backup=Path('/var/lib/semaphore/backups')/('operator-ui-'+args.action+'-'+str(time.time_ns()))
backup.mkdir(mode=0o700,parents=True)
digest=lambda path:hashlib.sha256(Path(path).read_bytes()).hexdigest()
manifest_path=Path('/opt/semaphore/release-tools-manifest.json')
if args.action=='inspector':
    target=Path('/opt/semaphore/playbooks/tools/release/inspect_runtime.py')
    shutil.copy2(target,backup/target.name)
    shutil.copy2(root/'inspect-tests/inspect_runtime.py',target)
    if manifest_path.exists():
        shutil.copy2(manifest_path,backup/manifest_path.name)
        m=json.loads(manifest_path.read_text());m['files']['tools/release/inspect_runtime.py']=digest(target)
        manifest_path.write_text(json.dumps(m,indent=2))
    print(json.dumps({'backup':str(backup),'services_changed':False}));sys.exit(0)

binary=root/'semaphore-ui'
assert args.sha256 and digest(binary)==args.sha256,'explicit binary hash mismatch'
current=Path('/opt/semaphore/bin/semaphore')
reported=subprocess.check_output([str(current),'version'],text=True).strip()
assert reported.startswith(('v2.19.12','2.19.12')),'not the pinned upstream version'
cfg=Path('/etc/semaphore/config.json')
settings=json.loads(cfg.read_text())
assert settings['sqlite']['host']=='/var/lib/semaphore/semaphore.sqlite'
db=Path(settings['sqlite']['host'])
shutil.copy2(current,backup/'semaphore')
shutil.copy2(cfg,backup/'config.json')
def schema(path):
    with sqlite3.connect('file:'+str(path)+'?mode=ro',uri=True) as c:
        assert c.execute('PRAGMA quick_check').fetchall()==[('ok',)]
        return c.execute("SELECT type,name,sql FROM sqlite_master ORDER BY type,name").fetchall()
subprocess.run(['systemctl','stop','semaphore.service'],check=True)
try:
    before=schema(db)
    with sqlite3.connect('file:'+str(db)+'?mode=ro',uri=True) as src,sqlite3.connect(backup/'semaphore.sqlite') as dst:src.backup(dst)
    staged=current.with_name('semaphore.ui-new');shutil.copy2(binary,staged);staged.chmod(0o755);os.replace(staged,current)
    subprocess.run(['systemctl','start','semaphore.service'],check=True)
    for attempt in range(30):
        try:
            with urllib.request.urlopen(settings['web_host']+'/api/ping',timeout=3) as r:assert r.read()==b'pong'
            break
        except Exception:
            if attempt==29:raise
            time.sleep(1)
    assert schema(db)==before,'unexpected database schema change'
    assert digest(cfg)==digest(backup/'config.json'),'configuration changed'
except Exception:
    subprocess.run(['systemctl','stop','semaphore.service'],check=True)
    shutil.copy2(backup/'semaphore',current)
    # Same-schema frontend patch rollback only; do not overwrite user data.
    if schema(db)==before:subprocess.run(['systemctl','start','semaphore.service'],check=True)
    raise
record={'backup':str(backup),'old_version':reported,'new_binary_sha256':digest(current),
        'ui_version':'20260930','upstream':'v2.19.12','database_schema_unchanged':True,'opc_services_changed':False}
Path('/opt/semaphore/operator-ui-record.json').write_text(json.dumps(record,indent=2))
print(json.dumps(record))
