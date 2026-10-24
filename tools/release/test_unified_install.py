import unittest
from pathlib import Path
from unified_install import agent_start, fixed_quote, stop_condition, independent_manager


class UnifiedInstallationTests(unittest.TestCase):
    def test_preserves_original_child_arguments_and_only_changes_control_listener(self):
        original='[Service]\nExecStart=/opt/rcd/bin/RcdAgent agent run --instance-id real-lower --data-dir /var/lib/rcd/agent --http-listen=127.0.0.1:8090 --lower-config-dir /etc/rcd/lower\n'
        changed=agent_start(original,Path('/opt/rcd/bin/RcdAgent'),Path('/opt/rcd/bin'),['192.168.10.30','192.168.10.58','192.168.10.46'])
        self.assertIn('"--lower-config-dir" "/etc/rcd/lower"',changed)
        self.assertIn('"--instance-id" "real-lower"',changed)
        self.assertEqual(changed.count('--http-listen'),1)
        self.assertIn('"0.0.0.0:8090"',changed)
        self.assertIn('"--remote-managers"',changed)
        self.assertNotIn('127.0.0.1',changed)

    def test_ambiguous_unit_or_specifiers_rejected(self):
        for text in ['ExecStart=/bin/sh -c anything', 'ExecStart=/opt/rcd/bin/RcdAgent agent run\nExecStart=/bin/true',
                     'ExecStart=/opt/rcd/bin/RcdAgent agent run --instance-id %i']:
            with self.assertRaises(ValueError): agent_start(text,Path('/opt/rcd/bin/RcdAgent'),Path('/opt/rcd/bin'),['127.0.0.1'])
        with self.assertRaises(ValueError):fixed_quote('x\n[Service]')

    def test_manual_stop_condition_is_not_shell_quoted(self):
        self.assertEqual(stop_condition(Path('/var/lib/rcd/manager/manual-stop.json')),
                         '!/var/lib/rcd/manager/manual-stop.json')
        for invalid in ('relative', '/path with spaces/hold', '/%i/hold', '/a\nOther='):
            with self.assertRaises(ValueError):stop_condition(invalid)

    def test_remove_only_declared_agent_dependencies(self):
        original = '[Unit]\nWants=network-online.target\nAfter=network-online.target\n[Service]\nRestart=on-failure\n[Unit]\nWants=agent.service\nAfter=agent.service another.service\n'
        result = independent_manager(original, 'agent.service')
        self.assertEqual(result, '[Unit]\nWants=network-online.target\nAfter=network-online.target\n[Service]\nRestart=on-failure\n[Unit]\nAfter=another.service\n')
        self.assertEqual(independent_manager(original, 'unrelated.service'), original)
        self.assertEqual(independent_manager('[Unit]\nWants=agent.service\nWants=\nWants=network-online.target\n', 'agent.service'), '[Unit]\nWants=\nWants=network-online.target\n')


if __name__=='__main__':unittest.main()
