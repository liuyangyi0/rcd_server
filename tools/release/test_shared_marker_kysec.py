"""No live labels are changed: isolated atomic-marker regression tests."""
import errno
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from deployment import durable_json


class SharedMarkerLabelTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.control = self.root / '.rcd-host-control'
        self.control.mkdir()
        self.marker = self.root / '.rcd-host-release-active.json'

    def exercise(self, existing):
        if existing:
            self.marker.write_text('{"state":"succeeded"}')
        labels = {self.control: b'none:none:trusted:999',
                  self.marker: b'none:none:unknown:999'}

        def read(path, *_args, **_kwargs):
            return labels.get(Path(path), b'none:none:unknown:9999')

        def write(path, name, value, **_kwargs):
            self.assertEqual(name, 'security.ksip')
            self.assertEqual(value, b'none:none:unknown:999')
            labels[Path(path)] = value

        with patch('deployment.os.getxattr', side_effect=read), patch('deployment.os.setxattr', side_effect=write) as setter:
            durable_json(self.marker, {'state': 'running'})
            setter.assert_called_once()
        self.assertEqual(json.loads(self.marker.read_text())['state'], 'running')

    def test_existing_label_survives_replace(self):
        self.exercise(True)

    def test_first_marker_uses_directory_kid_not_trust_flag(self):
        self.exercise(False)

    def test_permission_failure_preserves_original_marker(self):
        self.marker.write_text('original')
        with patch('deployment.os.getxattr', side_effect=PermissionError(errno.EACCES, 'denied')):
            with self.assertRaises(PermissionError):
                durable_json(self.marker, {'state': 'running'})
        self.assertEqual(self.marker.read_text(), 'original')
        self.assertEqual(list(self.root.glob('*.tmp')), [])

    def test_no_kysec_support(self):
        with patch('deployment.os.getxattr', side_effect=OSError(errno.ENOTSUP, 'unsupported')):
            durable_json(self.marker, {'state': 'succeeded'})
        self.assertEqual(json.loads(self.marker.read_text())['state'], 'succeeded')


if __name__ == '__main__':
    unittest.main()
