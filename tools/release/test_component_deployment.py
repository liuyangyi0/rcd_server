"""Isolated publisher state tests: no live services, serial or remote traffic."""
import json
from pathlib import Path
import sqlite3
import tempfile
import unittest
import subprocess
from unittest.mock import patch
from types import SimpleNamespace

from component_deployment import ComponentDeployment, compatible
from contextlib import closing
from deployment import require_no_unfinished_release, durable_json
from release import Release, sha, SoftwareAcceptanceError


class ManagerFixture(Release):
    def local_host(self): pass
    def preflight(self): return {'pid': self.pid()}
    def pid(self): return 88 if self.running else 0
    def control(self, action): self.calls.append(action); self.running = action == 'start'
    def accept(self, expected):
        if self.fail and self.entry.read_bytes() == b'new': raise SoftwareAcceptanceError('injected startup failure')
        if not self.running or sha(self.entry) != expected: raise RuntimeError('image mismatch')
        return {'binary_sha256': expected, 'pid': 88}


class ManagerDeploymentFixture(ComponentDeployment):
    def manager_request(self, payload=None):
        if payload:
            self.release.fence.update(active=payload['action'] == 'enter', deployment_id=payload['deployment_id'])
            self.release.fence['revision'] += 1
        return dict(self.release.fence, drained=True)


class ComponentDeploymentTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(); self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        for name in ('bin','source','runtime','releases'): (root/name).mkdir()
        self.config = root/'runtime/config.toml'; self.config.write_text('current-user-data')
        db = root/'runtime/manager.sqlite'
        with closing(sqlite3.connect(db)) as conn: conn.execute('pragma user_version=3')
        profile = {'host':'192.0.2.1','component':'manager','runtime_dir':str(root/'runtime'),
                   'source_dir':str(root/'source'),'release_dir':str(root/'releases'),
                   'executable':str(root/'bin/manager'),'databases':[str(db)],'config_files':[str(self.config)]}
        self.r = ManagerFixture(profile,profile['host'],'manager')
        self.r.running,self.r.fail,self.r.calls=True,False,[]
        self.r.fence={'instance_id':'m','deployment_id':'','revision':1,'active':False}
        self.r.entry.write_bytes(b'old')
        self.artifact=root/'releases/new';self.artifact.mkdir();(self.artifact/'binary').write_bytes(b'new')
        self.manifest={'component':'manager','host':profile['host'],'binary_sha256':sha(self.artifact/'binary'),
                       'build_info':{'version':'1.0.1'},'compatibility':{'adapter_min_version':'1.0.0','state_formats':{'manager_db':{'min':3,'max':3}},'component_min_versions':{}}}
        durable_json(self.artifact/'manifest.json',self.manifest)
        self.d=ManagerDeploymentFixture(self.r,'one')

    def test_upgrade_preserves_current_config_and_persists_stages(self):
        result=self.d.deploy(self.artifact,'operator')
        self.assertEqual(result['state'],'succeeded');self.assertEqual(self.config.read_text(),'current-user-data')
        self.assertEqual(self.r.calls,['stop','start']);self.assertFalse(self.r.fence['active'])
        self.assertEqual(self.d.record['stages'][-1]['stage'],'maintenance_completed')

    def stopped_communicate(self):
        previous = self.r.entry
        self.r.entry = previous.with_name('rcd_communicate')
        previous.rename(self.r.entry)
        self.r.p.update(component='communicate', databases=[],
                        executable=str(self.r.entry),
                        control={'kind': 'systemd-user', 'unit': 'rcd-communicate.service'})
        self.r.databases = []
        self.r.running = False
        self.r.systemctl = lambda *args: 'LoadState=loaded\nActiveState=inactive\nSubState=dead\nMainPID=0\nJob='
        self.manifest.update(component='communicate', compatibility={
            'adapter_min_version':'1.0.0','state_formats':{},'component_min_versions':{}})
        durable_json(self.artifact/'manifest.json',self.manifest)
        return ComponentDeployment(self.r, 'communication')

    def test_stopped_communication_upgrade_never_starts_or_stops_another_component(self):
        d = self.stopped_communicate()
        with patch.object(d, 'manager_action', side_effect=AssertionError('not a manager')), \
             patch.object(self.r, 'json_request', side_effect=AssertionError('not a lower Agent')):
            result = d.deploy(self.artifact, 'operator')
        self.assertEqual(result['state'], 'succeeded')
        self.assertEqual(result['acceptance']['state'], 'installed_stopped')
        self.assertEqual(self.r.calls, [])
        self.assertEqual(self.config.read_text(), 'current-user-data')
        self.assertIsNone(d.lower)
        self.assertEqual(self.r.entry.read_bytes(), b'new')

    def test_running_communication_is_rejected_before_any_stop_or_replacement(self):
        d = self.stopped_communicate(); self.r.running = True
        with self.assertRaisesRegex(RuntimeError, '排空'):
            d.deploy(self.artifact, 'operator')
        self.assertEqual(self.r.entry.read_bytes(), b'old')
        self.assertEqual(self.r.calls, [])
        self.assertFalse(d.host_marker.exists())

    def test_communication_stopped_state_must_remain_proven_before_install(self):
        d = self.stopped_communicate()
        self.r.systemctl = lambda *args: 'LoadState=loaded\nActiveState=activating\nSubState=auto-restart\nMainPID=0\nJob='
        with self.assertRaisesRegex(RuntimeError, '未确认停止'):
            d.deploy(self.artifact, 'operator')
        self.assertEqual(self.r.entry.read_bytes(), b'old')
        self.assertEqual(self.r.calls, [])

    def test_communication_shares_host_lock_with_all_existing_components(self):
        import fcntl
        d = self.stopped_communicate()
        with (self.r.entry.parent/'.rcd-host-release.lock').open('a') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaises(BlockingIOError):
                d.deploy(self.artifact, 'operator')
        self.assertEqual(self.r.entry.read_bytes(), b'old')
        self.assertEqual(self.r.calls, [])

    def generic_application(self):
        self.r.application = True
        self.r.p.update(component='my-app', databases=[], control={'kind':'systemd-user','unit':'rcd-app-my-app.service'})
        self.r.databases=[]
        # This fixture tests the transaction state machine in a temp directory;
        # fixed /opt ownership/profile validation is covered separately.
        formats=patch('component_deployment.state_versions',return_value={})
        formats.start();self.addCleanup(formats.stop)
        self.r.require_application_service=lambda: {}
        self.manifest.update(component='my-app', compatibility={
            'adapter_min_version':'1.0.0','state_formats':{},'component_min_versions':{}})
        durable_json(self.artifact/'manifest.json',self.manifest)
        d=ComponentDeployment(self.r,'generic')
        d.ensure_no_process_images=lambda targets: None
        return d

    def test_registered_application_upgrade_uses_only_own_normal_service_control(self):
        d=self.generic_application()
        with patch.object(d,'manager_action',side_effect=AssertionError('not manager')), \
             patch.object(self.r,'json_request',side_effect=AssertionError('not Agent')):
            result=d.deploy(self.artifact,'operator')
        self.assertEqual(result['state'],'succeeded')
        self.assertEqual(self.r.calls,['stop','start'])
        self.assertEqual(self.config.read_text(),'current-user-data')
        self.assertFalse(d.record['lower_was_running'])
        self.assertFalse((self.r.runtime/'manual-stop.json').exists())

    def test_registered_application_confirmed_start_failure_recovers_binary_only(self):
        d=self.generic_application();self.r.fail=True
        result=d.deploy(self.artifact,'operator')
        self.assertEqual(result['state'],'recovered')
        self.assertEqual(self.r.entry.read_bytes(),b'old')
        self.assertEqual(self.config.read_text(),'current-user-data')

    def test_registered_application_stopped_before_upgrade_stays_stopped(self):
        d=self.generic_application();self.r.running=False
        result=d.deploy(self.artifact,'operator')
        self.assertEqual(result['acceptance']['state'],'installed_stopped')
        self.assertEqual(self.r.calls,[])
        self.assertEqual(json.loads((self.r.runtime/'manual-stop.json').read_text())['desired_state'],'stopped')

    def test_registered_application_stop_timeout_does_not_replace_or_retry(self):
        d=self.generic_application()
        def fail_stop(action):
            self.r.calls.append(action)
            raise subprocess.TimeoutExpired('systemctl',30)
        self.r.control=fail_stop
        with self.assertRaises(subprocess.TimeoutExpired):d.deploy(self.artifact,'operator')
        self.assertEqual(self.r.calls,['stop'])
        self.assertEqual(self.r.entry.read_bytes(),b'old')
        self.assertEqual(d.record['state'],'needs_maintenance')
        self.assertTrue((self.r.runtime/'manual-stop.json').is_file())

    def test_generic_upgrade_inhibits_boot_before_stop_and_retains_hold_on_backup_crash(self):
        d=self.generic_application();control=self.r.control
        def protected_control(action):
            if action=='stop':
                hold=json.loads((self.r.runtime/'manual-stop.json').read_text())
                self.assertEqual(hold['operation_id'],d.id)
                self.assertEqual(hold['desired_state'],'stopped')
            return control(action)
        self.r.control=protected_control
        with patch.object(self.r,'backup',side_effect=OSError('crash boundary')):
            with self.assertRaises(OSError):d.deploy(self.artifact,'operator')
        self.assertEqual(self.r.calls,['stop'])
        self.assertEqual(self.r.entry.read_bytes(),b'old')
        self.assertEqual(json.loads((self.r.runtime/'manual-stop.json').read_text())['operation_id'],d.id)

    def test_generic_upgrade_keeps_original_manual_stop_marker_unchanged(self):
        d=self.generic_application();self.r.running=False
        hold=self.r.runtime/'manual-stop.json'
        original={'schema_version':1,'desired_state':'stopped','operation_id':'prior-manual-stop'}
        durable_json(hold,original);before=hold.read_bytes()
        d.deploy(self.artifact,'operator')
        self.assertEqual(hold.read_bytes(),before)
        self.assertEqual(self.r.calls,[])

    def test_generic_running_process_with_an_existing_stop_hold_is_rejected_without_clearing_it(self):
        d=self.generic_application()
        hold=self.r.runtime/'manual-stop.json'
        durable_json(hold,{'schema_version':1,'desired_state':'stopped','operation_id':'other-stop'})
        before=hold.read_bytes()
        with self.assertRaisesRegex(RuntimeError,'conflicts'):d.deploy(self.artifact,'operator')
        self.assertEqual(hold.read_bytes(),before)
        self.assertEqual(self.r.calls,[])
        self.assertEqual(self.r.entry.read_bytes(),b'old')

    def test_generic_unknown_start_response_restores_boot_hold_without_replaying(self):
        d=self.generic_application();control=self.r.control
        def lose_start(action):
            if action=='start':
                self.r.calls.append(action)
                raise TimeoutError('response lost')
            return control(action)
        self.r.control=lose_start
        with self.assertRaises(TimeoutError):d.deploy(self.artifact,'operator')
        self.assertEqual(self.r.calls,['stop','start'])
        self.assertEqual(d.record['state'],'unknown')
        self.assertTrue((self.r.runtime/'manual-stop.json').is_file())

    def test_previously_stopped_manager_stays_stopped(self):
        self.r.running=False
        result=self.d.deploy(self.artifact,'operator')
        self.assertEqual(result['acceptance']['state'],'installed_stopped');self.assertEqual(self.r.calls,[])

    def test_known_startup_failure_can_restore_binary_but_never_data(self):
        self.r.fail=True
        result=self.d.deploy(self.artifact,'operator')
        self.assertEqual(result['state'],'recovered');self.assertEqual(self.r.entry.read_bytes(),b'old')
        self.assertEqual(self.config.read_text(),'current-user-data');self.assertFalse(self.r.fence['active'])

    def test_systemctl_start_failure_recovers_only_with_definitive_stopped_evidence(self):
        original = self.r.control
        def fail_new(action):
            if action == 'start' and self.r.entry.read_bytes() == b'new':
                raise subprocess.CalledProcessError(1, ['systemctl','start','isolated'])
            return original(action)
        self.r.control = fail_new
        with patch.object(self.d, 'confirmed_service_stopped', return_value=True):
            result = self.d.deploy(self.artifact,'operator')
        self.assertEqual(result['state'],'recovered'); self.assertEqual(self.r.entry.read_bytes(),b'old')

    def test_unconfirmed_systemctl_failure_does_not_auto_restore(self):
        original = self.r.control
        def fail_new(action):
            if action == 'start': raise subprocess.CalledProcessError(1,['systemctl'])
            return original(action)
        self.r.control = fail_new
        with patch.object(self.d, 'confirmed_service_stopped', return_value=False):
            with self.assertRaises(subprocess.CalledProcessError): self.d.deploy(self.artifact,'operator')
        self.assertEqual(self.r.entry.read_bytes(),b'new');self.assertEqual(self.d.record['state'],'unknown')

    def test_unknown_reply_does_not_retry_or_restore(self):
        with patch.object(self.d,'manager_action',side_effect=TimeoutError('lost')):
            with self.assertRaises(TimeoutError):self.d.deploy(self.artifact,'operator')
        self.assertEqual(self.r.entry.read_bytes(),b'old');self.assertEqual(self.r.calls,[])
        with self.assertRaisesRegex(RuntimeError,'unfinished host'):require_no_unfinished_release(self.r)

    def test_incompatible_data_fails_before_admission_or_replacement(self):
        self.manifest['compatibility']['state_formats']['manager_db']['max']=2
        durable_json(self.artifact/'manifest.json',self.manifest)
        with self.assertRaisesRegex(RuntimeError,'incompatible'):self.d.deploy(self.artifact,'operator')
        self.assertFalse(self.d.host_marker.exists());self.assertEqual(self.r.calls,[])

    def test_newer_adapter_required_is_rejected(self):
        self.manifest['compatibility']['adapter_min_version']='9.0.0'
        with self.assertRaisesRegex(RuntimeError,'适配器'):compatible(self.r,self.manifest)

    def test_post_install_schema_change_blocks_auto_recovery(self):
        original=self.r.accept
        def changed(expected):
            with closing(sqlite3.connect(self.r.databases[0])) as conn:conn.execute('pragma user_version=4')
            raise SoftwareAcceptanceError('failure after migration')
        self.r.accept=changed
        with self.assertRaisesRegex(RuntimeError,'state format changed'):self.d.deploy(self.artifact,'operator')
        self.assertEqual(self.r.entry.read_bytes(),b'new');self.assertEqual(self.d.record['state'],'recovery_failed')

    def stopped_agent(self):
        r = self.r
        state = r.runtime/'process-state.json'
        state.write_text(json.dumps({'schema_version':2,'instance_id':'isolated-agent','desired_state':'stopped',
                                    'last_snapshot':{'state':'stopped','pid':0},'operations':[],
                                    'counter':73,'maintenance':{'phase':'completed','deployment_id':'original'}}))
        (r.runtime/'agent.lock').touch()
        lower = dict(r.p,component='lower',executable=str(r.entry.parent/'rcd_server'),
                     control={'kind':'process-agent','instance_id':'isolated-agent'})
        profile = r.runtime/'lower-profile.json';profile.write_text(json.dumps(lower))
        r.p.update(component='agent',lower_profile=str(profile),databases=[],
                   control={'kind':'systemd-user','unit':'isolated-agent.service'})
        r.databases=[];r.configs.append(state);r.running=False
        self.manifest.update(component='agent',compatibility={'adapter_min_version':'1.0.0',
                            'state_formats':{'agent_state':{'min':2,'max':2}},'component_min_versions':{}})
        durable_json(self.artifact/'manifest.json',self.manifest)
        d=ComponentDeployment(r,'offline')
        r.systemctl=lambda *args:'LoadState=loaded\nActiveState=inactive\nSubState=dead\nMainPID=0\nJob='
        return d,state

    def test_stopped_agent_binary_upgrade_keeps_all_state_bytes_and_does_not_start(self):
        d,state = self.stopped_agent(); before=state.read_bytes()
        with patch.object(d,'ensure_no_process_images'):
            result=d.deploy(self.artifact,'operator')
        self.assertEqual(result['state'],'succeeded');self.assertEqual(result['acceptance']['state'],'installed_stopped')
        self.assertEqual(state.read_bytes(),before);self.assertEqual(self.r.calls,[])

    def test_offline_agent_lock_contention_and_unfinished_job_refused(self):
        import fcntl
        d,state=self.stopped_agent()
        with (self.r.runtime/'agent.lock').open('r+') as lock:
            fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
            with self.assertRaises(BlockingIOError):d.deploy(self.artifact,'operator')
        value=json.loads(state.read_text());value['maintenance']['phase']='stopped';state.write_text(json.dumps(value))
        with self.assertRaisesRegex(RuntimeError,'停止状态'):d.deploy(self.artifact,'operator')
        self.assertEqual(self.r.entry.read_bytes(),b'old');self.assertEqual(self.r.calls,[])

    def test_service_failure_proof_rejects_auto_restart_or_pending_job(self):
        self.r.p['control']={'kind':'systemd-user','unit':'isolated.service'}
        for text,expected in [('ActiveState=failed\nSubState=failed\nMainPID=0\nJob=',True),
                              ('ActiveState=activating\nSubState=auto-restart\nMainPID=0\nJob=',False),
                              ('ActiveState=failed\nSubState=failed\nMainPID=0\nJob=32',False)]:
            with patch.object(self.r,'systemctl',return_value='LoadState=loaded\n'+text):self.assertEqual(self.d.confirmed_service_stopped(),expected)

    def test_lower_start_failure_requires_exact_terminal_request_and_retained_owner(self):
        result={'request_id':'one.start','action':'start','instance_id':'lower','state':'failed'}
        state={'pid':0,'state':'failed','pending_request_id':''}
        job={'lower_confirmed':True,'release_requested':False}
        self.d.lower=SimpleNamespace(release=SimpleNamespace(p={'control':{'instance_id':'lower'}},json_request=lambda _:result),
                                     process_snapshot=lambda:state,maintenance=lambda:job,owner=lambda value:value)
        self.assertTrue(self.d.confirmed_lower_start_failed('one.start'))
        self.assertFalse(self.d.confirmed_lower_start_failed('another.start'))
        for field,value in [('pid',10),('pending_request_id','pending')]:
            saved=state[field];state[field]=value;self.assertFalse(self.d.confirmed_lower_start_failed('one.start'));state[field]=saved
        job['release_requested']=True;self.assertFalse(self.d.confirmed_lower_start_failed('one.start'))

    def test_dead_agent_recovery_uses_durable_owner_not_unavailable_http(self):
        d,state=self.stopped_agent()
        d.lower.release.p.update(gateway_instance_id='gateway',maintenance_endpoint='fixed-endpoint')
        value=json.loads(state.read_text())
        value['maintenance']={'deployment_id':d.id,'instance_id':'isolated-agent','gateway_instance_id':'gateway',
                              'endpoint':'fixed-endpoint','target_version':'1.0.1','fencing_token':'7',
                              'phase':'stopped','lower_confirmed':True,'release_requested':False}
        state.write_text(json.dumps(value))
        backup=self.artifact/'backup';backup.mkdir();(backup/'binary').write_bytes(b'old')
        d.record={'backup':str(backup),'previous_binary_sha256':sha(backup/'binary'),
                  'initial_schemas':{'agent_state':2},'lower_was_running':True,'was_running':True,
                  'target_version':'1.0.1','fencing_token':'7'}
        d.lower.record=d.record;self.r.entry.write_bytes(b'new')
        with patch.object(d.lower,'drain',side_effect=AssertionError('dead Agent HTTP called')), \
             patch.object(d.lower,'process_action',side_effect=AssertionError('dead Agent HTTP called')), \
             patch.object(d,'ensure_no_process_images',side_effect=RuntimeError('orphan')):
            with self.assertRaisesRegex(RuntimeError,'orphan'):d.recover_binary()
        self.assertEqual(self.r.entry.read_bytes(),b'new')
        with patch.object(d.lower,'drain',side_effect=AssertionError('dead Agent HTTP called')), \
             patch.object(d.lower,'process_action',side_effect=AssertionError('dead Agent HTTP called')), \
             patch.object(d,'ensure_no_process_images') as absence, \
             patch.object(d,'restore_running',return_value={'binary_sha256':d.record['previous_binary_sha256']}), \
             patch.object(d,'complete'),patch.object(d,'mark'):
            self.assertEqual(d.recover_binary()['state'],'recovered')
            absence.assert_called_once()
        self.assertEqual(self.r.entry.read_bytes(),b'old')


if __name__=='__main__':unittest.main()
