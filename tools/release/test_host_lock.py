"""Host publication serialization, using only temporary directories."""
import fcntl
import os
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest

from deployment import target_lock


class HostLockTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)

    def release(self, name):
        return SimpleNamespace(entry=self.root / name)

    def test_components_exclude_each_other_and_release_after_exception(self):
        with target_lock(self.release('rcd_server')):
            for name in ('RcdManager', 'RcdAgent', 'rcd_server'):
                with self.assertRaises(BlockingIOError):
                    with target_lock(self.release(name)):
                        self.fail('competing publication admitted')
        with self.assertRaises(ValueError):
            with target_lock(self.release('RcdAgent')):
                raise ValueError('simulated failure')
        with target_lock(self.release('RcdManager')):
            pass

    def test_legacy_component_lock_is_still_respected(self):
        with (self.root / '.rcd_server.release.lock').open('w') as legacy:
            fcntl.flock(legacy, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaises(BlockingIOError):
                with target_lock(self.release('rcd_server')):
                    self.fail('old runner ignored')
        with target_lock(self.release('rcd_server')):
            pass

    def test_app_in_separate_directory_still_excludes_builtin_host_release(self):
        app_bin=self.root/'apps/my-app/bin';app_bin.mkdir(parents=True)
        app=SimpleNamespace(entry=app_bin/'my-app',p={'host_lock_dir':str(self.root)})
        with target_lock(self.release('RcdManager')):
            with self.assertRaises(BlockingIOError):
                with target_lock(app):self.fail('custom app bypassed shared host lock')
        with target_lock(app):
            with self.assertRaises(BlockingIOError):
                with target_lock(self.release('rcd_server')):self.fail('builtin bypassed application publication')

    def test_unsafe_lock_inode_is_rejected(self):
        lock = self.root / '.rcd-host-release.lock'
        target = self.root / 'unrelated'
        target.write_text('untouched')
        lock.symlink_to(target)
        with self.assertRaises(OSError):
            with target_lock(self.release('RcdAgent')):
                self.fail('symlink accepted')
        lock.unlink()
        os.link(target, lock)
        with self.assertRaises(RuntimeError):
            with target_lock(self.release('RcdAgent')):
                self.fail('hard link accepted')
        self.assertEqual(target.read_text(), 'untouched')


if __name__ == '__main__':
    unittest.main()
