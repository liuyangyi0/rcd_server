#!/usr/bin/env python3
"""Verify and import the previous recorded catalog as disabled historical packages.

Legacy binaries lack the native maintenance interface. They are retained with
an explicitly unsupported adapter requirement, never advertised as one-click
recovery targets. The original directories and files remain unchanged.
"""
import argparse
import getpass
import json
from pathlib import Path
import sys
import tempfile
import urllib.request
sys.path.insert(0,str(Path(__file__).resolve().parents[3]/'release'))
from rcd_package import export,digest
from onboard import Client


def import_catalog(api,project,catalog,work):
    bounds={'lower':{'command_journal':{'min':1,'max':1},'maintenance':{'min':0,'max':0}},
            'manager':{'manager_db':{'min':15,'max':15}},'agent':{'agent_state':{'min':1,'max':1}}}
    report=[]
    for component,formats in bounds.items():
        build=catalog/component
        with tempfile.TemporaryDirectory(prefix='legacy-import-',dir=work) as temp:
            archive=Path(temp)/(component+'.rcd.tar.gz')
            export(build,digest(build/'manifest.json'),
                   {'adapter_min_version':'999.0.0','state_formats':formats,'component_min_versions':{}},
                   '旧版接入档案：原目录与二进制摘要已核对。缺少新版维护接口，仅归档，不用于网页一键升级或恢复。',archive)
            with archive.open('rb') as body:
                request=urllib.request.Request(api.base+'/api/project/'+str(project)+'/rcd/packages',data=body,
                    headers={'Content-Type':'application/gzip','Content-Length':str(archive.stat().st_size),'Origin':api.base})
                with api.http.open(request,timeout=120) as response: package=json.load(response)
            if package['enabled']:
                package=api.request('/project/'+str(project)+'/rcd/packages/'+package['id']+'/enabled',
                    {'revision':package['revision'],'enabled':False},'PUT',{'If-Match':'"'+str(package['revision'])+'"'})
            report.append({'component':component,'id':package['id'],'enabled':package['enabled']})
    print(json.dumps(report))


if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--base-url',required=True)
    p.add_argument('--project-id',type=int,required=True)
    p.add_argument('--catalog',type=Path,required=True)
    p.add_argument('--work-dir',type=Path,required=True)
    a=p.parse_args()
    api=Client(a.base_url,'admin',getpass.getpass('Semaphore password: '))
    import_catalog(api,a.project_id,a.catalog.resolve(strict=True),a.work_dir.resolve(strict=True))
