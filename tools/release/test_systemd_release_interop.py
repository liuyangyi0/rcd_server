"""Opt-in real Agent/systemd test in the 46 isolated workspace; no serial ports.

Run as root with RCD_INTEROP_ROOT and RCD_INTEROP_AGENT set. Only a uniquely
named transient user unit is touched. This is not nine-component site acceptance.
"""
from contextlib import closing
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import time
import unittest
import urllib.request
import uuid

from component_deployment import ComponentDeployment
from deployment import durable_json
from release import Release, sha


@unittest.skipUnless(os.environ.get('RCD_INTEROP_ROOT') and os.environ.get('RCD_INTEROP_AGENT'), 'explicit isolated remote runtime required')
class RealStoppedAgentTests(unittest.TestCase):
    def test_real_agent_stopped_upgrade_and_real_systemctl_failure(self):
        workspace=Path(os.environ['RCD_INTEROP_ROOT']).resolve()
        binary=Path(os.environ['RCD_INTEROP_AGENT']).resolve()
        self.assertTrue(str(workspace).startswith(('/home/lenovo/code/rcd_semaphore_', '/home/lenovo/code/rcd_unified_')))
        self.assertTrue(str(binary).startswith(str(workspace)+'/'))
        self.assertEqual(os.geteuid(),0,'full /proc evidence requires the same root identity as Ansible become')
        with tempfile.TemporaryDirectory(prefix='agent-runtime-',dir=workspace) as temp:
            root=Path(temp);root.chmod(0o755)
            for name in ('bin','source','runtime','releases','lower-runtime'):(root/name).mkdir()
            entry=root/'bin/RcdAgent';shutil.copy2('/opt/rcd/bin/RcdAgent',entry)
            lower=root/'bin/rcd_server';shutil.copy2('/bin/false',lower)
            with closing(socket.socket()) as s:s.bind(('127.0.0.1',0));port=s.getsockname()[1]
            args=[str(entry),'agent','run','--instance-id','review-offline','--executable',str(lower),
                  '--work-dir',str(root/'lower-runtime'),'--data-dir',str(root/'runtime'),
                  '--http-listen','127.0.0.1:'+str(port),'--auto-start=false']
            with (root/'agent.log').open('w') as log:
                child=subprocess.Popen(args,stdout=log,stderr=subprocess.STDOUT)
                try:
                    deadline=time.monotonic()+20
                    while True:
                        try:
                            request=urllib.request.Request('http://127.0.0.1:'+str(port)+'/api/v1/process',headers={'X-RCD-Instance-ID':'review-offline'})
                            with urllib.request.urlopen(request,timeout=1) as response:state=json.load(response)
                            break
                        except OSError:
                            if time.monotonic()>deadline:raise
                            time.sleep(.2)
                    self.assertEqual(state['state'],'stopped');self.assertEqual(state.get('pid',0),0)
                finally:
                    child.terminate();child.wait(timeout=15)
            before=(root/'runtime/process-state.json').read_bytes()
            schema=json.loads(before)['schema_version']
            unit='rcd-review-'+uuid.uuid4().hex+'.service'
            user=['runuser','-u','lenovo','--','env','XDG_RUNTIME_DIR=/run/user/1000','DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus']
            try:
                # Actual systemd failed-start evidence, not a mocked return code.
                subprocess.run(user+['systemd-run','--user','--unit='+unit,'--property=Type=oneshot',str(entry),'agent','invalid-argument'],stdout=subprocess.PIPE,stderr=subprocess.STDOUT)
                profile={'host':'192.168.10.46','component':'agent','runtime_dir':str(root/'runtime'),
                         'source_dir':str(root/'source'),'release_dir':str(root/'releases'),'executable':str(entry),
                         'config_files':[str(root/'runtime/process-state.json')],
                         'control':{'kind':'systemd-user','unit':unit,'run_as':'lenovo'}}
                lower_profile=dict(profile,component='lower',runtime_dir=str(root/'lower-runtime'),executable=str(lower),config_files=[],
                                   control={'kind':'process-agent','instance_id':'review-offline','url':'http://127.0.0.1:'+str(port)})
                path=root/'lower-profile.json';path.write_text(json.dumps(lower_profile));profile['lower_profile']=str(path)
                release=Release(profile,profile['host'],'agent');d=ComponentDeployment(release,'real-stopped')
                deadline=time.monotonic()+10
                while not d.confirmed_service_stopped():
                    if time.monotonic()>deadline:self.fail('real systemd failed state not confirmed')
                    time.sleep(.2)
                with self.assertRaises(subprocess.CalledProcessError):release.control('start')
                self.assertTrue(d.confirmed_service_stopped())
                artifact=root/'releases/new';artifact.mkdir();shutil.copy2(binary,artifact/'binary')
                build=json.loads(subprocess.check_output([str(binary),'agent','version','--json'],text=True))
                durable_json(artifact/'manifest.json',{'host':profile['host'],'component':'agent','binary_sha256':sha(binary),
                    'build_info':build,'compatibility':{'adapter_min_version':'1.0.0','state_formats':{'agent_state':{'min':schema,'max':schema}},'component_min_versions':{}}})
                result=d.deploy(artifact,'isolated-real-agent-test')
                self.assertEqual(result['state'],'succeeded');self.assertEqual(result['acceptance']['state'],'installed_stopped')
                self.assertEqual(release.pid(),0);self.assertEqual(sha(entry),sha(binary))
                self.assertEqual((root/'runtime/process-state.json').read_bytes(),before)
                print('REAL Agent binary upgraded; Agent and lower remain stopped; durable state unchanged; real systemd failure classified')
            finally:
                subprocess.run(user+['systemctl','--user','stop',unit],stdout=subprocess.PIPE,stderr=subprocess.STDOUT)
                subprocess.run(user+['systemctl','--user','reset-failed',unit],stdout=subprocess.PIPE,stderr=subprocess.STDOUT)


if __name__=='__main__':unittest.main()
