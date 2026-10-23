#!/usr/bin/env python3
"""Provision native guest-only fleet readers; never echo or embed credentials.

Creates three native users/tokens through Semaphore APIs. Secrets are written to
an explicit private directory for the authorized host installer, not to reports.
Existing users without the matching receipt are not reset or taken over.
"""
import argparse
import getpass
import json
import os
from pathlib import Path
import secrets
from onboard import Client


def provision(api, base, project, directory):
    if not directory.is_absolute() or directory.is_symlink():
        raise ValueError('explicit private output directory required')
    directory.mkdir(mode=0o700, parents=False, exist_ok=True)
    if directory.stat().st_mode & 0o077:
        raise ValueError('token output directory must be private')
    existing = {u['username']: u for u in api.request('/users')}
    members = {u['username']:u for u in api.request('/project/'+str(project)+'/users')}
    results = []
    for suffix in ('30','58','46'):
        username = 'rcd-fleet-' + suffix
        path = directory/(username+'.token')
        receipt = directory/(username+'.json')
        if username in existing:
            if not receipt.is_file() or not path.is_file() or path.is_symlink() or path.stat().st_mode & 0o077:
                raise ValueError('existing reader lacks private receipt; inspect rather than reset: '+username)
            saved = json.loads(receipt.read_text())
            if (saved['user_id'] != existing[username]['id'] or existing[username].get('admin') or
                    members.get(username,{}).get('role') != 'guest' or saved['project_id'] != project):
                raise ValueError('existing reader identity/role differs')
            results.append(saved)
            continue
        password = secrets.token_urlsafe(40)
        user = api.request('/users',{'username':username,'name':'RCD '+suffix+' 台账只读',
            'email':username+'@rcd.invalid','password':password,'admin':False})
        api.request('/project/'+str(project)+'/users',{'user_id':user['id'],'role':'guest'})
        reader = Client(base, username, password)
        token = reader.request('/user/tokens',{'name':'RCD '+suffix+' fleet read-only'})
        with path.open('x') as f:
            os.fchmod(f.fileno(),0o600);f.write(token['id']+'\n');f.flush();os.fsync(f.fileno())
        saved = {'user_id':user['id'],'username':username,'project_id':project,'role':'guest',
                 'token_file':str(path)}
        with receipt.open('x') as f:
            os.fchmod(f.fileno(),0o600);json.dump(saved,f);f.flush();os.fsync(f.fileno())
        results.append(saved)
    return results


if __name__ == '__main__':
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--base-url',required=True)
    p.add_argument('--project-id',required=True,type=int)
    p.add_argument('--directory',required=True,type=Path)
    args=p.parse_args()
    api=Client(args.base_url,'admin',getpass.getpass('Semaphore password: '))
    print(json.dumps(provision(api,args.base_url,args.project_id,args.directory),ensure_ascii=False))
