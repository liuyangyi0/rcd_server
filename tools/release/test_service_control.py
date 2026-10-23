"""Isolated service transaction regression: no real systemd/OPC/serial writes."""
import json
from pathlib import Path
from unittest.mock import patch
from test_component_deployment import ComponentDeploymentTests, ManagerDeploymentFixture
from service_control import ServiceControl
from release import sha
from release_probe import completed_journal_report
from deployment import require_no_unfinished_release, durable_json


class ServiceControlTests(ComponentDeploymentTests):
    def setUp(self):
        super().setUp()
        self.r.p['control'] = {'kind': 'systemd-user', 'unit': 'isolated.service'}
        self.r.systemctl = self.systemctl

    def systemctl(self, *args):
        if args[0] == 'cat': return 'ConditionPathExists=!' + str(self.r.runtime / 'manual-stop.json')
        if 'SendSIGKILL' in args: return 'no'
        if args[0] == 'show': return 'LoadState=loaded\nMainPID=0\nActiveState=inactive\nSubState=dead\nJob=' if not self.r.running else 'LoadState=loaded\nMainPID=88\nActiveState=active\nSubState=running\nJob='
        if args[0] in ('start', 'stop'): return self.r.control(args[0])
        if args[0] in ('enable','daemon-reload'): return ''
        raise AssertionError(args)

    def operation(self, key, action):
        binding = {'operation_id': key * 64, 'device_id': 'd1', 'task_id': 1, 'host_alias': 'isolated'}
        op = ServiceControl(self.r, key * 64, action, sha(self.r.entry), 'operator', binding)
        op.d = ManagerDeploymentFixture(self.r, key * 64)
        return op

    def execute(self, op):
        real_sha = sha
        with patch('service_control.sha', side_effect=lambda p: real_sha(self.r.entry) if str(p) == '/proc/88/exe' else real_sha(p)):
            return op.execute()

    def test_stop_then_explicit_start_preserves_original_fence(self):
        stop = self.operation('a', 'stop'); result = self.execute(stop)
        self.assertEqual(result['state'], 'stopped'); self.assertTrue(stop.hold.exists())
        self.assertTrue(self.r.fence['active']); self.assertEqual(self.config.read_text(), 'current-user-data')
        # A new operation resumes the original stopped fence rather than entering another.
        start = self.operation('b', 'start'); result = self.execute(start)
        self.assertEqual(result['state'], 'running'); self.assertFalse(start.hold.exists())
        self.assertFalse(self.r.fence['active']); self.assertEqual(self.r.fence['deployment_id'], 'a' * 64)
        self.assertEqual(self.r.calls, ['stop', 'start'])

    def test_restart_one_component_without_install_or_other_process_actions(self):
        op = self.operation('a', 'restart'); before = self.r.entry.read_bytes()
        self.execute(op)
        self.assertEqual(self.r.calls, ['stop', 'start']); self.assertEqual(self.r.entry.read_bytes(), before)
        self.assertFalse(op.hold.exists()); self.assertFalse(self.r.fence['active'])
        report = completed_journal_report(self.r.p, dict(op.report_binding, component='manager'))
        self.assertEqual(report['acceptance']['state'], 'running')

    def test_lost_reply_never_stops_or_replays(self):
        op = self.operation('a', 'restart')
        with patch.object(op.d, 'manager_request', side_effect=TimeoutError('lost')):
            with self.assertRaises(TimeoutError): self.execute(op)
        self.assertEqual(self.r.calls, []); self.assertEqual(op.d.record['state'], 'needs_maintenance')
        with self.assertRaisesRegex(RuntimeError, 'unfinished host'): self.execute(self.operation('b', 'restart'))

    def test_unsafe_service_unit_rejected_before_intent(self):
        op = self.operation('a', 'stop')
        self.r.systemctl = lambda *args: 'yes' if 'SendSIGKILL' in args else 'ConditionPathExists=' + str(op.hold)
        with self.assertRaisesRegex(RuntimeError, '保护'): self.execute(op)
        self.assertEqual(self.r.calls, []); self.assertFalse(op.d.path.exists())

    def test_original_lifecycle_unknown_blocks_service_control(self):
        directory = self.r.entry.parent / '.rcd-host-control'; directory.mkdir()
        durable_json(directory / 'lifecycle.json', {'state': 'unknown'})
        with self.assertRaisesRegex(RuntimeError, 'lifecycle'): self.execute(self.operation('a', 'stop'))
        self.assertEqual(self.r.calls, [])

    def test_quoted_systemd_condition_is_rejected_instead_of_assumed_effective(self):
        op = self.operation('a', 'stop')
        self.r.systemctl = lambda *args: 'no' if 'SendSIGKILL' in args else 'ConditionPathExists="!' + str(op.hold) + '"'
        with self.assertRaisesRegex(RuntimeError, '保护'): self.execute(op)
        self.assertEqual(self.r.calls, [])

    def test_control_target_whitelist(self):
        for action in ('shell', 'kill', 'recover'):
            with self.assertRaises(ValueError): self.operation('a', action)

    def generic_operation(self,key,action):
        self.generic_application()
        op=self.operation(key,action)
        op.d.ensure_no_process_images=lambda targets:None
        return op

    def test_generic_stop_then_explicit_start_uses_no_rcd_maintenance_protocol(self):
        stop=self.generic_operation('a','stop')
        with patch.object(stop.d,'manager_request',side_effect=AssertionError('not manager')):
            result=self.execute(stop)
        self.assertEqual(result['state'],'stopped');self.assertTrue(stop.hold.exists())
        start=self.generic_operation('b','start')
        with patch.object(start.d,'manager_request',side_effect=AssertionError('not manager')):
            result=self.execute(start)
        self.assertEqual(result['state'],'running');self.assertFalse(start.hold.exists())
        self.assertEqual(self.r.calls,['stop','start'])
        self.assertFalse(self.r.fence['active'])

    def test_generic_restart_preserves_binary_and_configuration(self):
        op=self.generic_operation('a','restart');before=sha(self.r.entry)
        result=self.execute(op)
        self.assertEqual(result['state'],'running');self.assertEqual(self.r.calls,['stop','start'])
        self.assertEqual(sha(self.r.entry),before);self.assertEqual(self.config.read_text(),'current-user-data')

    def test_first_explicit_generic_start_loads_unit_only_after_durable_intent(self):
        op=self.generic_operation('a','start');self.r.running=False
        loaded=[];systemctl=self.r.systemctl
        def guarded_systemctl(*args):
            if args[0]=='daemon-reload':
                self.assertTrue(op.d.path.is_file())
                self.assertEqual(op.d.record['stages'][-1]['stage'],'application_unit_reload')
                self.assertEqual(op.d.record['stages'][-1]['state'],'intent')
                loaded.append(True)
            return systemctl(*args)
        self.r.systemctl=guarded_systemctl
        def preflight():
            self.assertEqual(loaded,[True])
            return {'pid':self.r.pid()}
        self.r.preflight=preflight
        result=self.execute(op)
        self.assertEqual(result['state'],'running');self.assertEqual(self.r.calls,['start'])

    def test_generic_stop_unconfirmed_does_not_force_kill_or_start(self):
        op=self.generic_operation('a','restart')
        with patch.object(op.d,'confirmed_service_stopped',return_value=False), \
             patch('service_control.time.monotonic',side_effect=[0,41]):
            with self.assertRaisesRegex(RuntimeError,'未强杀'):self.execute(op)
        self.assertEqual(self.r.calls,['stop'])
        self.assertEqual(op.d.record['state'],'needs_maintenance')
        self.assertTrue(op.hold.is_file())

    def test_communication_is_not_a_generic_lifecycle_escape(self):
        self.stopped_communicate()
        with self.assertRaises(ValueError):self.operation('a','start')

    def test_agent_recovery_keeps_manual_stop_and_does_not_touch_lower(self):
        d,state = self.stopped_agent(); before=state.read_bytes()
        self.r.systemctl=self.systemctl
        op=ServiceControl(self.r,'c'*64,'recover',sha(self.r.entry),'operator',{'operation_id':'c'*64,'device_id':'d1','task_id':1,'host_alias':'isolated'})
        with patch.object(op.d,'ensure_no_process_images'), patch.object(op.d.lower,'process_snapshot',return_value={'pid':0,'desired_state':'stopped'}):
            result=op.execute()
        self.assertEqual(result['state'],'running');self.assertEqual(self.r.calls,['start'])
        self.assertEqual(state.read_bytes(),before)

    def test_only_exact_agent_owner_can_recover_an_unfinished_lower_job(self):
        d,state=self.stopped_agent();v=json.loads(state.read_text())
        v['lifecycle']={'request_id':'pending','state':'running'};state.write_text(json.dumps(v))
        directory=self.r.entry.parent/'.rcd-host-control';directory.mkdir()
        durable_json(directory/'lifecycle.json',{'request_id':'pending','state':'unknown'})
        require_no_unfinished_release(self.r,allow_agent_recovery=True)
        with self.assertRaisesRegex(RuntimeError,'lifecycle'):require_no_unfinished_release(self.r)
        v['lifecycle']['request_id']='different';state.write_text(json.dumps(v))
        with self.assertRaisesRegex(RuntimeError,'lifecycle'):require_no_unfinished_release(self.r,allow_agent_recovery=True)
