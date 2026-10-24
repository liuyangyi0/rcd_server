#!/usr/bin/env python3
"""One-target, copy-before-switch Linux path migration. Does not edit business config.

Run as root on the explicit target with a reviewed, private profile. The existing
unit and service owner are retained through one removable systemd drop-in. This
requires a manually established control maintenance window, not a software fence.
"""
import argparse
import fcntl
import json
import os
from pathlib import Path
import pwd
import re
import shutil
import socket
import sqlite3
import subprocess
import time
import urllib.request

from release import absolute, atomic_copy, run, sha, within


def service_environment(content, role, instance):
    """Keep credentials/business options verbatim; rewrite only owned paths."""
    if role == 'lower-stack':
        paths = {'RCD_LOWER_EXECUTABLE': '/opt/rcd/bin/rcd_server',
                 'RCD_LOWER_WORK_DIR': '/var/lib/rcd/lower/' + instance,
                 'RCD_AGENT_DATA_DIR': '/var/lib/rcd/agent/' + instance}
    elif role == 'manager':
        paths = {'RCD_MANAGER_DATA_DIR': '/var/lib/rcd/manager/' + instance}
    else:
        raise ValueError('unsupported environment role')
    for key, value in paths.items():
        # Do not add parameters absent from the old environment. The service
        # uses explicit argv; this prevents misleading legacy path hints.
        content = re.sub(r'(?m)^' + key + r'=.*$', key + '=' + value, content)
    return content


def tree_manifest(root):
    result = {}
    for path in sorted(root.rglob('*')):
        if path.is_symlink() or not (path.is_file() or path.is_dir()):
            raise RuntimeError('unexpected symlink/special file: ' + str(path))
        if path.is_file():
            result[str(path.relative_to(root))] = sha(path)
    return result


def copy_verified(source, target):
    """Writer must be stopped; reject changes during copying, never merge trees."""
    if target.exists():
        raise RuntimeError('destination already exists; inspect previous run: ' + str(target))
    before = tree_manifest(source)
    shutil.copytree(source, target, copy_function=shutil.copy2)
    if before != tree_manifest(source) or before != tree_manifest(target):
        raise RuntimeError('source changed during migration or copy mismatch')
    return before


def check_databases(root):
    result = {}
    for path in root.rglob('*'):
        if path.is_file() and path.suffix in ('.db', '.sqlite'):
            with sqlite3.connect(path.as_uri() + '?mode=ro', uri=True) as db:
                if db.execute('PRAGMA quick_check').fetchone()[0] != 'ok':
                    raise RuntimeError('database integrity failure: ' + str(path))
                result[str(path.relative_to(root))] = db.execute('PRAGMA user_version').fetchone()[0]
    return result


def own_tree(path, uid, gid):
    os.chown(path, uid, gid)
    for item in path.rglob('*'):
        os.chown(item, uid, gid, follow_symlinks=False)


class Migration:
    def __init__(self, profile, host, role):
        self.p = profile
        if profile['host'] != host or profile['role'] != role or role not in ('lower-stack', 'manager'):
            raise ValueError('explicit host/role mismatch')
        if host not in run('hostname', '-I').split() or os.geteuid() != 0:
            raise RuntimeError('must run as root ON the named Linux host')
        for key in ('instance', 'deployment_id', 'release_id'):
            if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,127}', profile[key]):
                raise ValueError('invalid ' + key)
        if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.@-]*\.service', profile['unit']):
            raise ValueError('one exact service unit required')
        self.account = pwd.getpwnam(profile['account'])
        self.backup = Path('/var/backups/rcd') / profile['deployment_id'] / role
        self.release = Path('/opt/rcd/releases') / profile['release_id']
        self.dropin = absolute(profile['dropin'])
        expected_unit = (Path('/etc/systemd/system') if profile['scope'] == 'system' else
                         Path(self.account.pw_dir) / '.config/systemd/user') / (profile['unit'] + '.d')
        if self.dropin != expected_unit / '90-rcd-paths.conf':
            raise ValueError('unexpected unit override path')
        self.data = Path('/var/lib/rcd') / ('lower' if role == 'lower-stack' else 'manager') / profile['instance']
        self.agent = Path('/var/lib/rcd/agent') / profile['instance']
        self.logs = Path('/var/log/rcd') / ('lower' if role == 'lower-stack' else 'manager') / profile['instance']
        self.agent_logs = Path('/var/log/rcd/agent') / profile['instance']
        self.env = Path('/etc/rcd') / (role + '-' + profile['instance'] + '.env')
        self.record = {}

    def systemctl(self, *args):
        if self.p['scope'] == 'system':
            return run('systemctl', *args)
        if self.p['scope'] != 'user':
            raise ValueError('unsupported service scope')
        return run('runuser', '-u', self.p['account'], '--', 'env',
                   'XDG_RUNTIME_DIR=/run/user/' + str(self.account.pw_uid), 'systemctl', '--user', *args)

    def pid(self):
        return int(self.systemctl('show', self.p['unit'], '-p', 'MainPID', '--value'))

    def process(self):
        request = urllib.request.Request('http://127.0.0.1:8090/api/v1/process',
                                         headers={'X-RCD-Instance-ID': self.p['instance']})
        with urllib.request.urlopen(request, timeout=5) as response:
            return json.load(response)

    def save(self, phase):
        self.record['phase'] = phase
        self.record['updated_at'] = time.time()
        temp = self.backup / 'record.tmp'
        temp.write_text(json.dumps(self.record, indent=2))
        os.replace(temp, self.backup / 'record.json')
        print(json.dumps({'phase': phase, 'host': self.p['host'], 'role': self.p['role'], 'backup': str(self.backup)}), flush=True)

    def preflight(self):
        if self.dropin.exists() or self.backup.exists() or self.data.exists() or self.env.exists() or self.logs.exists():
            raise RuntimeError('migration destination/record already exists; inspect instead of repeating')
        if self.p['role'] == 'lower-stack' and (self.agent.exists() or self.agent_logs.exists()):
            raise RuntimeError('agent destination already exists')
        pid = self.pid()
        image = Path('/proc/' + str(pid) + '/exe')
        # An earlier atomic software update can leave this still-running role
        # on the old inode (46 historically shared the manager/agent entry).
        # Validate the exact recorded pathname and preserve the actual image,
        # not the newer file currently found at that entry.
        image_name = os.readlink(image) if pid else ''
        if image_name.endswith(' (deleted)'):
            image_name = image_name[:-10]
        expected_image = absolute(self.p.get('old_running_executable', self.p['old_executable'])).resolve()
        if not pid or Path(image_name) != expected_image:
            raise RuntimeError('service process does not match declared old executable')
        old_paths = ['old_data'] if self.p['role'] == 'manager' else ['old_config', 'old_pki', 'old_logs', 'old_agent_data']
        total = 0
        for key in old_paths:
            root = absolute(self.p[key])
            if not root.is_dir():
                raise RuntimeError('missing migration input: ' + key)
            tree_manifest(root)  # reject unreviewed links, not a configuration baseline
            total += sum(p.stat().st_size for p in root.rglob('*') if p.is_file())
        if shutil.disk_usage('/var').free < total * 3 + 512 * 1024 * 1024:
            raise RuntimeError('insufficient migration/backup space')
        for name, digest in self.p['artifacts'].items():
            if name not in ('rcd_server', 'RcdManager', 'RcdAgent') or sha(self.release / name) != digest:
                raise RuntimeError('artifact name/digest mismatch')
        if self.p['role'] == 'lower-stack':
            state = self.process()
            if state.get('pending_request_id') or state['state'] != 'running' or state['desired_state'] != 'running':
                raise RuntimeError('lower must be confirmed running without a pending process job')
            if state['executable'] != self.p['old_lower_executable']:
                raise RuntimeError('lower target mismatch')
        return {'pid': pid, 'unit': self.p['unit'], 'software_target_verified': True,
                'running_sha256': sha(image), 'entry_sha256': sha(self.p['old_executable'])}

    def install(self):
        before = self.preflight()
        self.backup.mkdir(parents=True, mode=0o700)
        os.chmod(self.backup.parent, 0o700)
        self.record = {'profile': self.p, 'before': before, 'migration_tool_sha256': sha(Path(__file__))}
        (self.backup / 'old-unit.txt').write_text(self.systemctl('cat', self.p['unit']))
        shutil.copy2('/proc/' + str(before['pid']) + '/exe', self.backup / 'old-binary')
        if sha(self.backup / 'old-binary') != before['running_sha256']:
            raise RuntimeError('service image changed before backup')
        shutil.copy2(self.p['old_executable'], self.backup / 'old-entry-binary')
        if self.p.get('old_env'):
            shutil.copy2(self.p['old_env'], self.backup / 'old.env')
        if self.p['role'] == 'lower-stack':
            shutil.copy2(self.p['old_lower_executable'], self.backup / 'old-lower')
            lower_pid = self.process()['pid']
        else:
            lower_pid = None
        self.save('prepared')
        self.systemctl('stop', self.p['unit'])
        if self.pid() or (lower_pid and Path('/proc/' + str(lower_pid)).exists()):
            raise RuntimeError('stop not confirmed; do not copy or restart')
        self.save('stopped')
        try:
            if self.p['role'] == 'lower-stack':
                trees = {'config': self.p['old_config'], 'pki': self.p['old_pki'],
                         'logs': self.p['old_logs'], 'agent': self.p['old_agent_data']}
            else:
                trees = {'manager': self.p['old_data']}
            self.record['copies'] = {}
            for label, old in trees.items():
                target = self.backup / label
                self.record['copies'][label] = copy_verified(Path(old), target)
                check_databases(target)
            self.save('backed-up')
            self.data.parent.mkdir(parents=True, exist_ok=True)
            self.logs.parent.mkdir(parents=True, exist_ok=True)
            if self.p['role'] == 'lower-stack':
                self.data.mkdir(mode=0o750)
                copy_verified(self.backup / 'config', self.data / 'config')
                copy_verified(self.backup / 'pki', self.data / 'pki')
                (self.data / 'state').mkdir()
                for name in ('operations.json', 'station_versions.json', '.station_versions.json.bak',
                             'command-journal.sqlite', 'command-journal.sqlite-wal', 'command-journal.sqlite-shm',
                             'maintenance.sqlite', 'maintenance.sqlite-wal', 'maintenance.sqlite-shm'):
                    file = self.data / 'config' / name
                    if file.exists():
                        file.rename(self.data / 'state' / name)
                copy_verified(self.backup / 'logs', self.logs)
                self.agent.parent.mkdir(parents=True, exist_ok=True)
                copy_verified(self.backup / 'agent', self.agent)
                self.agent_logs.mkdir(parents=True)
                for name in ('process.log', 'process.log.1', 'agent.log', 'agent.log.1'):
                    file = self.agent / name
                    if file.exists():
                        file.rename(self.agent_logs / name)
                own_tree(self.agent, self.account.pw_uid, self.account.pw_gid)
                own_tree(self.agent_logs, self.account.pw_uid, self.account.pw_gid)
            else:
                copy_verified(self.backup / 'manager', self.data)
                if (self.data / 'logs').exists():
                    (self.data / 'logs').rename(self.logs)
                else:
                    self.logs.mkdir()
                (self.data / 'logs').symlink_to(self.logs, target_is_directory=True)
            own_tree(self.data, self.account.pw_uid, self.account.pw_gid)
            own_tree(self.logs, self.account.pw_uid, self.account.pw_gid)
            Path('/opt/rcd/bin').mkdir(parents=True, exist_ok=True)
            for name in self.p['artifacts']:
                dest = Path('/opt/rcd/bin') / name
                if dest.exists():
                    raise RuntimeError('new executable entry already exists: ' + str(dest))
                atomic_copy(self.release / name, dest)
                dest.chmod(0o755)
            self.env.parent.mkdir(parents=True, exist_ok=True)
            content = Path(self.p['old_env']).read_text() if self.p.get('old_env') else ''
            self.env.write_text(service_environment(content, self.p['role'], self.p['instance']))
            self.env.chmod(0o600)
            os.chown(self.env, self.account.pw_uid, self.account.pw_gid)
            # These are reviewed deployment args, not HTTP/user business inputs.
            args = self.p['new_argv']
            for arg in args:
                if not re.fullmatch(r'[A-Za-z0-9_./:=+@,-]+', arg):
                    raise ValueError('unsupported systemd argument; use an explicitly reviewed unit')
            self.dropin.parent.mkdir(parents=True, exist_ok=True)
            self.dropin.write_text('[Service]\nEnvironmentFile=\nEnvironmentFile=' + str(self.env) +
                                   '\nWorkingDirectory=' + str(self.data) + '\nExecStart=\nExecStart=' + ' '.join(args) + '\n')
            self.save('installed')
            self.systemctl('daemon-reload')
            self.systemctl('start', self.p['unit'])
            result = self.accept()
            self.record['acceptance'] = result
            self.save('accepted')
            return result
        except Exception as exc:
            self.record['error'] = str(exc)
            self.save('failed-inspect-before-recovery')
            raise

    def accept(self):
        end = time.monotonic() + 90
        error = ''
        while time.monotonic() < end:
            try:
                pid = self.pid()
                name = 'RcdAgent' if self.p['role'] == 'lower-stack' else 'RcdManager'
                if not pid or sha('/proc/' + str(pid) + '/exe') != self.p['artifacts'][name]:
                    raise RuntimeError('service image mismatch')
                result = {'pid': pid, 'executable': str(Path('/proc/' + str(pid) + '/exe').resolve())}
                if self.p['role'] == 'lower-stack':
                    state = self.process()
                    if state['state'] != 'running' or state['executable'] != '/opt/rcd/bin/rcd_server':
                        raise RuntimeError('child not running in new path')
                    if sha('/proc/' + str(state['pid']) + '/exe') != self.p['artifacts']['rcd_server']:
                        raise RuntimeError('child image mismatch')
                    result['lower_pid'] = state['pid']
                for port in self.p['ports']:
                    with socket.create_connection(('127.0.0.1', port), timeout=2):
                        pass
                result['ports'] = self.p['ports']
                return result
            except (OSError, ValueError, RuntimeError, KeyError) as exc:
                error = str(exc)
                time.sleep(1)
        raise RuntimeError('software acceptance failed: ' + error)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['preflight', 'install'])
    parser.add_argument('--profile', required=True)
    parser.add_argument('--host', required=True)
    parser.add_argument('--role', required=True, choices=['lower-stack', 'manager'])
    parser.add_argument('--confirm-no-active-commands', action='store_true')
    args = parser.parse_args()
    if args.action == 'install' and not args.confirm_no_active_commands:
        parser.error('requires a current, manually established control maintenance window')
    runner = Migration(json.loads(Path(args.profile).read_text()), args.host, args.role)
    # Host-wide deployment lock; serializes installer invocations, not commands.
    with open('/run/lock/rcd-path-migration.lock', 'a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        print(json.dumps(getattr(runner, args.action)()))


if __name__ == '__main__':
    main()
