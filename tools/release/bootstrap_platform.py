#!/usr/bin/env python3
"""One-time, explicitly authorized legacy capability upgrade; not normal release.

The old installation has no maintenance interface. This requires a current
operator-controlled command pause, records that exception, stops accepted work
before replacing binaries, and never restores old data or replays device commands.
"""
import argparse
from contextlib import closing
import json
import os
from pathlib import Path
import pwd
import re
import shutil
import sqlite3
import subprocess
import time

from deployment import durable_json, require_no_unfinished_release, target_lock
from release import Release, atomic_copy, sha

HOSTS = {'192.168.10.30': ('RCD93','rcd93-30'), '192.168.10.58': ('lenovo','rcd92-58'),
         '192.168.10.46': ('lenovo','real-lower')}


def run(host, artifacts, confirmed):
    if not confirmed or host not in HOSTS or os.geteuid()!=0:
        raise RuntimeError('explicit target, root and current command pause required')
    if host not in subprocess.check_output(['hostname','-I'], text=True).split():
        raise RuntimeError('must run on the named host')
    login, instance = HOSTS[host]
    profiles = {c:json.loads(Path('/etc/rcd/release-profiles',c+'.json').read_text()) for c in ('lower','manager','agent')}
    releases = {c:Release(p,host,c) for c,p in profiles.items()}
    manifests = {}
    for c in releases:
        build=artifacts/(c+'-1')
        m=json.loads((build/'manifest.json').read_text())
        if m['component']!=c or sha(build/'binary')!=m['binary_sha256'] or m['build_info']['version']!='0.6.2-ops.20261008.1':
            raise RuntimeError('unexpected recorded bootstrap artifact')
        manifests[c]=m
    root=Path('/var/backups/rcd')/('native-platform-bootstrap-'+time.strftime('%Y%m%d-%H%M%S'))
    root.mkdir(parents=True,mode=0o700)
    record={'host':host,'kind':'one_time_legacy_capability_upgrade','command_pause_confirmed':True,
            'state':'preflight','stages':[],'components':{c:m['binary_sha256'] for c,m in manifests.items()}}
    def stage(name, fn):
        record['stages'].append({'name':name,'state':'intent','at':time.time()})
        durable_json(root/'record.json',record)
        result=fn()
        record['stages'][-1].update(state='confirmed',result=result)
        durable_json(root/'record.json',record)
        return result
    # The common host lock is taken once; all three legacy markers are checked.
    with target_lock(releases['agent']):
        for r in releases.values():require_no_unfinished_release(r)
        agent=releases['agent']; lower=releases['lower']; manager=releases['manager']
        process=lower.json_request('/api/v1/process')
        if process.get('pending_request_id') or process['state'] not in ('running','stopped'):
            raise RuntimeError('lower process state is not settled')
        before={c:r.preflight() for c,r in releases.items()}
        if not before['agent']['pid']:raise RuntimeError('bootstrap requires the existing Agent online')
        argv=Path('/proc',str(before['agent']['pid']),'cmdline').read_bytes().rstrip(b'\0').decode().split('\0')
        if argv[0]!=str(agent.entry) or argv[1:3]!=['agent','run']:
            raise RuntimeError('unexpected Agent executable binding')
        if any(v.startswith(('--maintenance-endpoint','--gateway-instance-id')) for v in argv):
            raise RuntimeError('already maintenance-enabled; use normal native publishing')
        with closing(sqlite3.connect((lower.runtime/'state/command-journal.sqlite').as_uri()+'?mode=ro',uri=True)) as db:
            gateway=db.execute('select value from identity where id=1').fetchone()[0]
            if db.execute('select count(*) from commands where finished=0').fetchone()[0]:
                raise RuntimeError('accepted device commands still pending')
        if not re.fullmatch(r'[A-Za-z0-9_.-]+',gateway):raise RuntimeError('invalid stable lower identity')
        newdir=Path('/etc/rcd/release-platform')
        identitydir=Path('/opt/rcd/releases')/manifests['lower']['build_info']['release_id']
        if agent.p['control']['kind']=='systemd-user':
            unitdir=Path(pwd.getpwnam(login).pw_dir)/'.config/systemd/user'/ (agent.p['control']['unit']+'.d')
        else:unitdir=Path('/etc/systemd/system')/(agent.p['control']['unit']+'.d')
        drop=unitdir/'95-rcd-maintenance.conf'
        if newdir.exists() or identitydir.exists() or drop.exists():
            raise RuntimeError('bootstrap already has state; inspect original operation, do not rerun')
        argv += ['--maintenance-endpoint=opc.tcp://127.0.0.1:4841','--gateway-instance-id='+gateway]
        if any(any(x in v for x in ('\n','\r','%','$','"','\\')) for v in argv):raise RuntimeError('review unsafe service argument escaping')
        marker=agent.entry.parent/'.rcd-host-release-active.json'
        if marker.exists():shutil.copy2(marker,root/'previous-host-marker.json')
        durable_json(marker,{'deployment_id':root.name,'state':'needs_maintenance','record':str(root/'record.json')})
        record['before']=before
        shutil.copytree('/etc/rcd/release-profiles',root/'original-profiles')
        if process['state']=='running':stage('stop_lower',lambda:lower.control('stop'))
        stage('stop_agent',lambda:agent.control('stop'))
        if before['manager']['pid']:stage('stop_manager',lambda:manager.control('stop'))
        for c,r in releases.items():
            if c!='lower' and r.pid():raise RuntimeError('service did not stop')
            path=stage('backup_'+c,lambda r=r: str(r.backup(root)))
            record.setdefault('backups',{})[c]=path
        # The child must really be gone; no second serial reader is started.
        oldpid=before['lower']['pid']
        if oldpid and Path('/proc',str(oldpid)).exists():raise RuntimeError('old lower process has not exited')
        newdir.mkdir(mode=0o700)
        for c,p in profiles.items():
            p=dict(p,release_dir='/var/lib/rcd-release-platform/artifacts/'+c)
            if c=='lower':p.update(gateway_instance_id=gateway,maintenance_endpoint='opc.tcp://127.0.0.1:4841')
            if c=='agent':p['lower_profile']=str(newdir/'lower.json')
            Path(p['release_dir']).mkdir(parents=True,mode=0o700)
            durable_json(newdir/(c+'.json'),p)
        unitdir.mkdir(parents=True,exist_ok=True)
        drop.write_text('[Service]\nExecStart=\nExecStart='+' '.join('"'+v+'"' for v in argv)+'\n')
        stage('reload_service_binding',lambda:agent.systemctl('daemon-reload'))
        identitydir.mkdir(mode=0o755)
        identities={}
        for c,r in releases.items():
            stage('install_'+c,lambda r=r,c=c:atomic_copy(artifacts/(c+'-1')/'binary',r.entry,executable=True))
            shutil.copy2(r.entry,identitydir/r.entry.name)
            identities[r.entry.name]={'sha256':manifests[c]['binary_sha256'],'build_info':manifests[c]['build_info']}
        (identitydir/'identity-public.json').write_text(json.dumps({'components':identities}))
        stage('start_agent',lambda:agent.control('start'))
        stage('accept_agent',lambda:agent.accept(manifests['agent']['binary_sha256']))
        # Legacy stop intentionally persisted stopped; restore only what was running.
        if process['state']=='running':stage('start_lower',lambda:lower.control('start'))
        if process['state']=='running':stage('accept_lower',lambda:lower.accept(manifests['lower']['binary_sha256']))
        if before['manager']['pid']:
            stage('start_manager',lambda:manager.control('start'))
            stage('accept_manager',lambda:manager.accept(manifests['manager']['binary_sha256']))
        maintenance=lower.json_request('/api/v1/maintenance')
        if maintenance.get('supported') is not True or maintenance.get('gateway_instance_id')!=gateway:
            raise RuntimeError('new maintenance binding was not confirmed')
        if not (manager.runtime/'release-control.sock').exists():raise RuntimeError('manager release socket missing')
        record['state']='succeeded'
        durable_json(root/'record.json',record)
        durable_json(marker,{'deployment_id':root.name,'state':'succeeded','record':str(root/'record.json')})
        print(json.dumps({'host':host,'backup':str(root),'state':'succeeded','kind':record['kind']}))


if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--host',choices=HOSTS,required=True)
    p.add_argument('--artifacts',type=Path,required=True)
    p.add_argument('--confirm-no-active-commands',action='store_true')
    a=p.parse_args()
    run(a.host,a.artifacts.resolve(strict=True),a.confirm_no_active_commands)
