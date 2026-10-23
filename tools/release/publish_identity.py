"""Publish only verified non-secret software identity for unprivileged inventory.

Does not change private manifest permissions, programs, configs or services.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path

FIELDS=('schema_version','component','version','release_id','commit','built_at','source_sha256','dirty','target')
def public_identity(directory):
    result={'schema_version':1,'components':{}}
    for name in ('manifest.json','manager30-supplement.json'):
        path=directory/name
        if not path.is_file():continue
        manifest=json.loads(path.read_text())
        for binary,component in manifest.get('components',{}).items():
            if binary not in ('RcdManager','RcdAgent','rcd_server','rcd_communicate'):continue
            artifact=directory/binary
            if artifact.is_symlink():raise ValueError('artifact symlink rejected')
            with artifact.open('rb') as stream:
                h=hashlib.sha256()
                for block in iter(lambda:stream.read(1024*1024),b''):h.update(block)
            if h.hexdigest()!=component['sha256']:raise ValueError('artifact checksum mismatch')
            result['components'][binary]={'sha256':h.hexdigest(),'build_info':{
                key:value for key,value in component['build_info'].items() if key in FIELDS}}
    if not result['components']:raise ValueError('no verified identities')
    return result

if __name__=='__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--host',required=True)
    parser.add_argument('--release',required=True)
    args=parser.parse_args()
    import re,subprocess
    if args.host not in subprocess.check_output(['hostname','-I'],text=True).split():raise ValueError('wrong host')
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]+',args.release):raise ValueError('invalid release id')
    directory=Path('/opt/rcd/releases')/args.release
    target=directory/'identity-public.json'
    result=public_identity(directory)
    if target.exists() and json.loads(target.read_text())!=result:raise ValueError('immutable identity conflict')
    temporary=directory/'identity-public.json.new'
    with temporary.open('x') as out:json.dump(result,out,indent=2);out.flush();os.fsync(out.fileno())
    temporary.chmod(0o644)
    os.replace(temporary,target)
    print(json.dumps({'host':args.host,'public_identity':str(target),'components':list(result['components']),'services_changed':False}))
