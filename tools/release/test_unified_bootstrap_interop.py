"""Remote-only real manager + Agent + Rust wiring; no CSV or physical serial."""
from contextlib import closing
import json
import os
from pathlib import Path
import pwd
import shutil
import socket
import sqlite3
import subprocess
import tempfile
import time
import unittest
from unittest.mock import patch
import uuid

from deployment import durable_json
from release import Release, sha
from unified_bootstrap import Bootstrap
from unified_install import agent_start, fixed_quote, dropin


@unittest.skipUnless(os.environ.get('RCD_INTEROP_ROOT') and os.environ.get('RCD_INTEROP_MANAGER'), 'explicit remote isolation required')
class RealBootstrapTests(unittest.TestCase):
    def test_wiring_preserves_identity_and_resumes_original_fences(self):
        workspace = Path(os.environ['RCD_INTEROP_ROOT']).resolve(strict=True)
        binary = Path(os.environ['RCD_INTEROP_MANAGER']).resolve(strict=True)
        self.assertTrue(str(workspace).startswith('/home/lenovo/code/rcd_unified_'))
        self.assertTrue(str(binary).startswith(str(workspace) + '/'))
        self.assertEqual(os.geteuid(), 0)
        user = pwd.getpwnam('lenovo')
        prefix = ['runuser','-u',user.pw_name,'--','env','XDG_RUNTIME_DIR=/run/user/'+str(user.pw_uid),
                  'DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/'+str(user.pw_uid)+'/bus','systemctl','--user']
        def control(*args): return subprocess.check_output(prefix+list(args),stderr=subprocess.STDOUT,text=True)
        def port():
            with closing(socket.socket()) as s: s.bind(('127.0.0.1',0)); return s.getsockname()[1]
        with tempfile.TemporaryDirectory(prefix='wiring-',dir=workspace) as tmp:
            root=Path(tmp);root.chmod(0o755)
            for name in ('bin','source','lower','lower/config','lower/state','lower/pki','lower/log','agent','manager','releases'):
                path=root/name;path.mkdir();path.chmod(0o755);os.chown(path,user.pw_uid,user.pw_gid)
            for name in ('RcdManager','RcdAgent'): shutil.copy2(binary,root/'bin'/name)
            shutil.copy2('/opt/rcd/bin/rcd_server',root/'bin/rcd_server')
            for path in (root/'bin').iterdir():path.chmod(0o755)
            ahttp,lpc,mhttp,mpc=port(),port(),port(),port()
            gateway='wiring-'+uuid.uuid4().hex
            config=root/'lower/config/config.toml'
            config.write_text('[server]\nprimary_ip="127.0.0.1:'+str(port())+'"\nsecondary_ip="127.0.0.1:'+str(port())+'"\nrun_on="primary"\ncurrent_run="primary"\n[serial]\nbaud_rate=28800\ndata_bits="Eight"\nparity="None"\nstop_bits="One"\nflow_control="none"\n[settings]\ntimeout=50\npoll_interval_ms=5\ncommand_retry_count=3\nparity_switch_delay_ms=5\ntransmit_settle_guard_ms=5\n[calculation]\nmode="manager"\n')
            dbpath=root/'lower/state/command-journal.sqlite'
            with closing(sqlite3.connect(dbpath)) as db:
                db.execute('CREATE TABLE identity(id INTEGER PRIMARY KEY CHECK(id=1),value TEXT NOT NULL)')
                db.execute('INSERT INTO identity VALUES(1,?)',(gateway,));db.commit()
            os.chown(dbpath,user.pw_uid,user.pw_gid)
            host=subprocess.check_output(['hostname','-I'],text=True).split()[0]
            agent_args=[str(root/'bin/RcdAgent'),'agent','run','--instance-id','wiring-agent','--executable',str(root/'bin/rcd_server'),
                '--work-dir',str(root/'lower'),'--data-dir',str(root/'agent'),'--http-listen','127.0.0.1:'+str(ahttp),
                '--ready-address','127.0.0.1:'+str(lpc),'--maintenance-endpoint','opc.tcp://127.0.0.1:'+str(lpc),'--gateway-instance-id',gateway]
            for name in ('config','state','pki','log'):agent_args += ['--lower-'+name+'-dir',str(root/'lower'/name)]
            manager_args=[str(root/'bin/RcdManager'),'run','--data-dir',str(root/'manager'),'--http-listen','127.0.0.1:'+str(mhttp),
                          '--opc-listen','127.0.0.1:'+str(mpc),'--opc-advertise-host','127.0.0.1']
            units={c:'rcd-unified-wiring-'+c+'-'+uuid.uuid4().hex+'.service' for c in ('agent','manager')}
            profiles={}
            for c,args in (('agent',agent_args),('manager',manager_args)):
                path=root/units[c]
                # The fixture deliberately contains the legacy manager -> Agent
                # startup dependency. The installer must remove only that edge.
                deps='Wants='+units['agent']+'\nAfter='+units['agent']+'\n' if c=='manager' else ''
                path.write_text('[Unit]\n'+deps+'[Service]\nType=simple\nEnvironment=RCD_OPCUA_PORT='+str(lpc)+'\nSendSIGKILL=no\nExecStart='+ ' '.join(fixed_quote(a) for a in args)+'\n')
                path.chmod(0o644)
                (root/'releases'/c).mkdir()
                profiles[c]=dict(host=host,component=c,source_dir=str(root/'source'),release_dir=str(root/'releases'/c),
                    executable=str(root/'bin'/('RcdAgent' if c=='agent' else 'RcdManager')),runtime_dir=str(root/c),
                    control={'kind':'systemd-user','unit':units[c],'run_as':user.pw_name},
                    opc_port=lpc if c=='agent' else mpc,health_url='http://127.0.0.1:'+str(ahttp if c=='agent' else mhttp)+'/healthz',
                    config_files=[str(root/'agent/process-state.json')] if c=='agent' else [],
                    databases=[] if c=='agent' else [str(root/'manager/manager.db')])
            profiles['lower']=dict(host=host,component='lower',source_dir=str(root/'source'),release_dir=str(root/'releases'),
                executable=str(root/'bin/rcd_server'),runtime_dir=str(root/'lower'),opc_port=lpc,
                control={'kind':'process-agent','instance_id':'wiring-agent','url':'http://127.0.0.1:'+str(ahttp)},
                gateway_instance_id=gateway,maintenance_endpoint='opc.tcp://127.0.0.1:'+str(lpc),
                databases=[str(dbpath),str(root/'lower/state/maintenance.sqlite')],config_files=[str(config)])
            profiles['agent']['lower_profile']=str(root/'lower.json')
            profile_paths={c:str(root/(c+'.json')) for c in profiles}
            for c,value in profiles.items():durable_json(Path(profile_paths[c]),value)
            token=root/'manager/token';token.write_text('isolated-nonfunctional-native-token');token.chmod(0o600);os.chown(token,user.pw_uid,user.pw_gid)
            fleet={'schema_version':1,'platform_url':'http://127.0.0.1:'+str(port()),'project_id':1,
                'token_file':str(token),'source_devices':{'a':'a','b':'b','c':'c'}}
            original=config.read_bytes()
            try:
                for unit in units.values():
                    installed=Path(user.pw_dir)/'.config/systemd/user'/unit
                    self.assertFalse(installed.exists())
                    shutil.copy2(root/unit,installed)
                control('daemon-reload');control('start',units['manager'])
                for c in ('agent','manager'):Release(profiles[c],host,c).accept(sha(binary))
                lower=Release(profiles['lower'],host,'lower')
                until=time.monotonic()+60
                while lower.json_request('/api/v1/process')['state']!='running':
                    if time.monotonic()>until: self.fail('isolated Rust not ready')
                    time.sleep(.25)
                b=Bootstrap(profile_paths,host,['192.168.10.30','192.168.10.58','192.168.10.46'],fleet,'isolated-wiring',sha(binary))
                # Only the listening port differs from production, to avoid its
                # 8090 listener. All process/gate/systemd behavior remains real.
                def isolated_listener(*args):return agent_start(*args).replace('0.0.0.0:8090','127.0.0.1:'+str(ahttp))
                with patch('unified_install.agent_start',side_effect=isolated_listener):result=b.execute('isolated-operator')
                self.assertEqual(result['state'],'succeeded')
                status=lower.json_request('/api/v1/process')
                self.assertTrue(status['lifecycle_supported']);self.assertEqual(status['gateway_instance_id'],gateway)
                self.assertEqual(status['state'],'running');self.assertFalse(status.get('maintenance_deployment_id'))
                self.assertNotIn(units['agent'],control('show',units['manager'],'-p','Wants','--value'))
                self.assertEqual(config.read_bytes(),original)
                self.assertFalse(b.manager.manager_request()['active'])
                with patch('unified_install.agent_start',side_effect=isolated_listener):
                    with self.assertRaisesRegex(RuntimeError,'original installation exists'):b.execute('isolated-operator')
                durable_json(workspace/'real-unified-wiring-result.json',{'state':'passed','serial_devices':False,'live_units_touched':False,'config_preserved':True})
            finally:
                for c in ('manager','agent'):
                    control('stop',units[c])
                    link=Path(user.pw_dir)/'.config/systemd/user'/units[c]
                    if link.is_file() and not link.is_symlink():link.unlink()
                    directory=dropin(Release(profiles[c],host,c)).parent
                    if directory.is_dir():
                        for name in ('79-rcd-graceful.conf','zz-rcd-unified.conf'):
                            file=directory/name
                            if file.is_file():file.unlink()
                        directory.rmdir()
                control('daemon-reload')


if __name__=='__main__':unittest.main()
