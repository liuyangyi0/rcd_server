#!/usr/bin/env python3
"""Publish a new immutable native playbook snapshot; never mutate one in use."""
import argparse
import getpass
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import urllib.error
from onboard import Client


def publish(source, project, repository, api):
    if os.geteuid()!=0 or '192.168.10.46' not in subprocess.check_output(['hostname','-I'],text=True).split():
        raise RuntimeError('root on 46 required')
    prefix='/project/'+str(project)
    page = 1
    while True:
        operations=api.request(prefix+'/rcd/operations?page_size=100&page='+str(page))
        if any(o['state'] not in ('succeeded','recovered','partial_failed','failed_before_install','failed_before_execution','cancelled') for o in operations['items']):
            raise RuntimeError('finish or reconcile active publication before updating tools')
        if page >= operations['total_pages']:
            break
        page += 1
    try:
        setups=api.request(prefix+'/rcd/host-setups')
        if any(s['state'] not in ('installed_stopped','failed','host_key_conflict','cancelled') for s in setups['items']):
            raise RuntimeError('finish or reconcile active host setup before updating tools')
    except urllib.error.HTTPError as error:
        if error.code!=404: raise
    files={}
    for folder in ('tools/release','tools/ansible'):
        for path in (source/folder).rglob('*'):
            if path.is_symlink():raise RuntimeError('tool snapshot refuses links')
            if path.is_file() and '__pycache__' not in path.parts:
                files[str(path.relative_to(source))]=hashlib.sha256(path.read_bytes()).hexdigest()
    key=hashlib.sha256(json.dumps(files,sort_keys=True).encode()).hexdigest()
    destination=Path('/opt/semaphore/rcd-platform')/('tools-'+key[:20])
    if destination.exists():
        for name,expected in files.items():
            if hashlib.sha256((destination/name).read_bytes()).hexdigest()!=expected:raise RuntimeError('existing snapshot differs')
    else:
        destination.mkdir(mode=0o755)
        for name in files:
            target=destination/name;target.parent.mkdir(parents=True,exist_ok=True)
            shutil.copyfile(source/name,target);target.chmod(0o644)
            if hashlib.sha256(target.read_bytes()).hexdigest()!=files[name]:
                raise RuntimeError('source changed while creating immutable snapshot')
        (destination/'manifest.json').write_text(json.dumps(files,indent=2))
    repo=api.request(prefix+'/repositories/'+str(repository))
    old=repo['git_url'];repo['git_url']=str(destination)
    api.request(prefix+'/repositories/'+str(repository),repo,'PUT')
    record={'previous_snapshot':old,'snapshot':str(destination),'sha256':key,'repository_id':repository,'project_id':project}
    (destination/'registration.json').write_text(json.dumps(record,indent=2))
    print(json.dumps(record))


if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--source',type=Path,required=True)
    p.add_argument('--project-id',type=int,required=True)
    p.add_argument('--repository-id',type=int,required=True)
    a=p.parse_args()
    api=Client('http://192.168.10.46:3000','admin',getpass.getpass('Semaphore password: '))
    publish(a.source.resolve(strict=True),a.project_id,a.repository_id,api)
