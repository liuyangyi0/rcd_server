"""Explicit remote-only real manager/systemd interop; no live units or sources.

Requires root, RCD_INTEROP_ROOT and RCD_INTEROP_MANAGER in the 46 isolated
workspace. The only service installed is a UUID-named temporary user unit.
It is stopped and unlinked in finally; business units/configs are never read.
"""
from contextlib import closing
import json
import os
from pathlib import Path
import pwd
import shutil
import socket
import subprocess
import tempfile
import unittest
import uuid

from deployment import durable_json
from release import Release, sha
from service_control import ServiceControl
from unified_install import fixed_quote, stop_condition


@unittest.skipUnless(os.environ.get('RCD_INTEROP_ROOT') and os.environ.get('RCD_INTEROP_MANAGER'), 'explicit isolated manager binary required')
class RealServiceControlTests(unittest.TestCase):
    def test_manager_stop_start_restart_with_durable_manual_stop(self):
        workspace = Path(os.environ['RCD_INTEROP_ROOT']).resolve(strict=True)
        binary = Path(os.environ['RCD_INTEROP_MANAGER']).resolve(strict=True)
        self.assertTrue(str(workspace).startswith('/home/lenovo/code/rcd_unified_'))
        self.assertTrue(str(binary).startswith(str(workspace) + '/'))
        self.assertEqual(os.geteuid(), 0, 'match Ansible become for process evidence')
        user = pwd.getpwnam('lenovo')
        prefix = ['runuser', '-u', user.pw_name, '--', 'env', 'XDG_RUNTIME_DIR=/run/user/'+str(user.pw_uid),
                  'DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/'+str(user.pw_uid)+'/bus', 'systemctl', '--user']
        def systemctl(*args):
            return subprocess.run(prefix + list(args), check=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        def port():
            with closing(socket.socket()) as probe:
                probe.bind(('127.0.0.1', 0)); return probe.getsockname()[1]
        with tempfile.TemporaryDirectory(prefix='manager-control-', dir=workspace) as temp:
            root = Path(temp); root.chmod(0o755)
            for name in ('bin', 'source', 'runtime', 'releases'):
                (root/name).mkdir(); (root/name).chmod(0o755)
            os.chown(root/'runtime', user.pw_uid, user.pw_gid)
            entry = root/'bin/RcdManager'; shutil.copy2(binary, entry); entry.chmod(0o755)
            http, opc = port(), port()
            unit = 'rcd-unified-test-' + uuid.uuid4().hex + '.service'
            unit_path = root/unit
            hold = root/'runtime/manual-stop.json'
            args = [str(entry), 'run', '--data-dir', str(root/'runtime'), '--http-listen', '127.0.0.1:'+str(http),
                    '--opc-listen', '127.0.0.1:'+str(opc), '--opc-advertise-host', '127.0.0.1']
            unit_path.write_text('[Unit]\nConditionPathExists=' + stop_condition(hold) +
                '\n[Service]\nType=simple\nSendSIGKILL=no\nTimeoutStopSec=20\nExecStart=' +
                ' '.join(fixed_quote(a) for a in args) + '\n')
            unit_path.chmod(0o644)
            host = subprocess.check_output(['hostname','-I'], text=True).split()[0]
            profile = dict(host=host, component='manager', executable=str(entry), runtime_dir=str(root/'runtime'),
                source_dir=str(root/'source'), release_dir=str(root/'releases'), databases=[str(root/'runtime/manager.db')],
                config_files=[], control={'kind':'systemd-user','unit':unit,'run_as':user.pw_name},
                health_url='http://127.0.0.1:'+str(http)+'/healthz', opc_port=opc)
            release = Release(profile,host,'manager')
            expected = sha(entry)
            try:
                systemctl('link',str(unit_path));systemctl('daemon-reload');systemctl('start',unit)
                release.accept(expected)
                for i, action in enumerate(('stop','start','restart')):
                    key = str(i+1)*64
                    operation = ServiceControl(release,key,action,expected,'isolated-test',
                        {'operation_id':key,'device_id':'isolated-manager','host_alias':'isolated','task_id':i+1})
                    before = operation.d.manager_request() if release.pid() else None
                    result = operation.execute()
                    if action == 'stop':
                        self.assertEqual(result['state'],'stopped');self.assertEqual(release.pid(),0)
                        self.assertTrue(hold.is_file())
                        identity = before['instance_id']
                        self.assertEqual(json.loads((root/'runtime/release-maintenance.json').read_text())['active'],True)
                        # A generic boot/service start cannot undo an operator stop.
                        systemctl('start',unit);self.assertEqual(release.pid(),0)
                    else:
                        self.assertGreater(release.pid(),0);self.assertFalse(hold.exists())
                        status = operation.d.manager_request()
                        self.assertEqual(status['instance_id'],identity);self.assertFalse(status['active'])
                    pid = release.pid()
                    with self.assertRaisesRegex(RuntimeError,'already recorded'): operation.execute()
                    self.assertEqual(release.pid(),pid,'same operation must not restart again')
                ansible = os.environ.get('RCD_ANSIBLE_PLAYBOOK')
                if ansible:
                    key = 'a'*64
                    report_dir = root/'native-reports';report_dir.mkdir()
                    profile_path = root/'profile.json';durable_json(profile_path,profile)
                    inventory = root/'inventory.json'
                    durable_json(inventory,{'all':{'children':{'rcd_nodes':{'hosts':{'isolated':{
                        'ansible_connection':'local','ansible_host':host,'ansible_python_interpreter':'/usr/bin/python3',
                        'rcd_release_profiles':{'manager':str(profile_path)}}}}}}})
                    variables = root/'variables.json'
                    durable_json(variables,{'rcd_platform_workspace':str(root/'ansible-tools'), 'rcd_platform_control':{
                        'operation_id':key,'task_id':4,'component':'manager','action':'restart','actor':'isolated-native',
                        'commands_paused':True,'hosts':['isolated'],'report_directory':str(report_dir),
                        'targets':[{'device_id':'isolated-manager','host_alias':'isolated','address':host,'expected_binary_sha256':expected}]}})
                    playbook = Path(__file__).resolve().parents[1]/'ansible/control-platform.yml'
                    ran = subprocess.run([ansible,'-i',str(inventory),str(playbook),'-e','@'+str(variables)],
                        text=True,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,timeout=180)
                    self.assertEqual(ran.returncode,0,ran.stdout)
                    result = json.loads((report_dir/'isolated-manager.json').read_text())
                    self.assertEqual(result['state'],'succeeded');self.assertEqual(result['operation_id'],key)
                    self.assertEqual(result['acceptance']['state'],'running')
                    self.assertEqual(result['acceptance']['binary_sha256'],expected)
                durable_json(workspace/'real-manager-service-result.json',
                    {'result':'passed','actions':['stop','blocked-generic-start','start','restart'],
                     'ansible_control':bool(ansible), 'binary_sha256':expected,'live_units_touched':False,'serial_devices':False})
            finally:
                systemctl('stop',unit)
                link = Path(user.pw_dir)/'.config/systemd/user'/unit
                if link.is_symlink() and link.resolve() == unit_path:
                    link.unlink()
                systemctl('daemon-reload')


if __name__ == '__main__': unittest.main()
