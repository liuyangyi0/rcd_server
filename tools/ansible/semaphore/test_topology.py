import unittest
from unittest.mock import patch
import connect_release


class DeploymentTopologyTest(unittest.TestCase):
    def test_all_three_hosts_have_all_three_components(self):
        for suffix in ('30', '46', '58'):
            self.assertEqual(connect_release.target_components(suffix), ['lower', 'agent', 'manager'])

    def test_unknown_target_rejected(self):
        with self.assertRaises(ValueError):
            connect_release.target_components('99')

    def test_export_catalog_includes_manager_on_30(self):
        import tempfile
        import json
        from pathlib import Path
        original = Path
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            def redirected(path):
                return root / str(path).lstrip('/')
            manifest = {'components': {}, 'source': {}, 'identity': {'commit': 'test'}}
            for component, name in connect_release.COMPONENTS.items():
                binary = redirected('/opt/rcd/bin') / name
                binary.parent.mkdir(parents=True, exist_ok=True)
                binary.write_bytes(name.encode())
                manifest['components'][name] = {'sha256': connect_release.digest(binary), 'build_info': {'component': component}}
            recorded = redirected('/opt/rcd/releases') / connect_release.RELEASE / 'manifest.json'
            recorded.parent.mkdir(parents=True)
            recorded.write_text(json.dumps(manifest))
            def resolve(path):
                text = str(path)
                return original(path) if text.startswith(temporary) else redirected(path)
            with patch.object(connect_release, 'Path', side_effect=resolve):
                connect_release.export_catalog()
            result = json.loads(redirected('/opt/semaphore/catalog/releases.json').read_text())
            for target in connect_release.TARGETS:
                self.assertIn('manager', result['targets']['192.168.10.' + target]['components'])


if __name__ == '__main__':
    unittest.main()
