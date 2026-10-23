#!/usr/bin/env python3
"""Explicit, single-host Linux release runner. Never run against live services
without current deployment authorization. No fleet defaults or stored passwords.
"""
import argparse
from contextlib import closing
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import socket
import sqlite3
import subprocess
import time
import urllib.request
import urllib.parse
import uuid
from deployment import (Deployment, durable_json, sync_directory, target_lock,
                        require_no_unfinished_release)
from application_profile import BUILTINS, component_slug, validate_application_profile, unit_text


def run(*args, cwd=None, env=None):
    return subprocess.check_output(args, cwd=cwd, env=env, text=True, stderr=subprocess.STDOUT).strip()


def build_identity(commit, source_sha, release_id, version):
    for name, value in [('release_id', release_id), ('version', version)]:
        if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9._+\-]{0,127}', value):
            raise ValueError(name + ' must be a nonempty safe build identifier')
    return {'version': version, 'commit': commit, 'release_id': release_id,
            'source_sha256': source_sha, 'dirty': True,
            'built_at': datetime.now(timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')}


def identity_environment(identity):
    env = os.environ.copy()
    for field, name in [('version', 'RCD_BUILD_VERSION'), ('commit', 'RCD_BUILD_COMMIT'),
                        ('release_id', 'RCD_RELEASE_ID'), ('source_sha256', 'RCD_SOURCE_SHA256'),
                        ('built_at', 'RCD_BUILD_TIME')]:
        env[name] = identity[field]
    env['RCD_BUILD_DIRTY'] = 'true'
    return env


def identity_ldflags(identity):
    fields = [('version', 'Version'), ('commit', 'Commit'), ('built_at', 'BuiltAt'),
              ('release_id', 'ReleaseID'), ('source_sha256', 'SourceSHA256')]
    return ' '.join('-X rcd_manager/internal/buildinfo.' + dest + '=' + identity[src]
                    for src, dest in fields) + ' -X rcd_manager/internal/buildinfo.Dirty=true'


def sha(file):
    with Path(file).open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest() if hasattr(hashlib, 'file_digest') else digest_stream(stream)


def digest_stream(stream):
    h = hashlib.sha256()
    for chunk in iter(lambda: stream.read(1024 * 1024), b''):
        h.update(chunk)
    return h.hexdigest()


def source_manifest(source):
    names = ['src', 'crates', 'tests', 'test_data', 'vendor', 'serialport-rs', 'Cargo.toml', 'Cargo.lock', 'build.rs', 'rust-toolchain.toml',
             'manager/internal', 'manager/cmd', 'manager/third_party', 'manager/go.mod',
             'manager/go.sum', 'manager/web', 'communicate', 'contracts', 'tools/release', 'tools/ansible', 'tools/mender']
    files = {}
    for name in names:
        base = source / name
        candidates = base.rglob('*') if base.is_dir() else [base]
        for file in candidates:
            if any(x in {'.git', 'target', 'node_modules', 'dist', '__pycache__', 'coverage', 'test-results'} for x in file.relative_to(source).parts):
                continue
            if file.is_file():
                if file.is_symlink():
                    raise RuntimeError('source manifest refuses unrecorded symlink: ' + str(file))
                files[str(file.relative_to(source))] = sha(file)
    encoded = json.dumps(files, sort_keys=True, separators=(',', ':')).encode()
    return {'sha256': hashlib.sha256(encoded).hexdigest(), 'files': files}


def require_manager_web_sources(source):
    """Reject partial/stale console snapshots before a release build starts."""
    web = source / 'manager/web'
    required = ['src/App.tsx', 'src/components/AppShell.tsx', 'src/pages/HelpPage.tsx',
                'src/pages/HelpPage.css', 'src/features/help/content.ts',
                'src/features/help/screenshots.ts', 'src/features/help/RelationshipDiagram.tsx',
                'embed_help_test.go']
    for name in required:
        if not (web / name).is_file():
            raise RuntimeError('incomplete web source snapshot; missing manager/web/' + name)
    if 'HelpPage' not in (web / 'src/App.tsx').read_text() or "'/help'" not in (web / 'src/components/AppShell.tsx').read_text():
        raise RuntimeError('help page route/navigation missing from web source snapshot')
    images = set(re.findall(r"id: '((?:manager|platform)-[a-z-]+)'", (web / 'src/features/help/screenshots.ts').read_text()))
    if not images:
        raise RuntimeError('help screenshot manifest is empty')
    for name in images:
        image = web / 'public/help/screenshots' / (name + '.jpg')
        if not image.is_file() or image.is_symlink():
            raise RuntimeError('missing help screenshot: ' + name)
        with image.open('rb') as stream:
            if stream.read(3) != b'\xff\xd8\xff':
                raise RuntimeError('invalid help screenshot: ' + name)
    return len(images)


def absolute(value):
    p = Path(value)
    if not p.is_absolute() or p.resolve() in (Path('/'), Path.home().resolve()):
        raise ValueError('an explicit non-root absolute path is required: ' + value)
    return p


def within(path, parent):
    try:
        path.resolve().relative_to(parent.resolve())
        return True
    except ValueError:
        return False


def sqlite_backup(source, destination):
    with closing(sqlite3.connect(source.as_uri() + '?mode=ro', uri=True)) as src, closing(sqlite3.connect(destination)) as dst:
        with dst:
            src.backup(dst)
            if dst.execute('PRAGMA quick_check').fetchone()[0] != 'ok':
                raise RuntimeError('SQLite backup integrity failure')
            return dst.execute('PRAGMA user_version').fetchone()[0]


def atomic_copy(source, destination, executable=False):
    temporary = destination.with_name(destination.name + '.release-' + uuid.uuid4().hex)
    try:
        shutil.copy2(source, temporary)
        # Publishing as root must not turn service-owned configuration into
        # root-owned files during an explicitly requested data rollback.
        if destination.exists() and os.geteuid() == 0:
            owner = destination.stat()
            os.chown(temporary, owner.st_uid, owner.st_gid)
        if executable:
            temporary.chmod(0o755)
        with temporary.open('rb') as stream:
            os.fsync(stream.fileno())
        os.replace(temporary, destination)
        sync_directory(destination.parent)
    finally:
        temporary.unlink(missing_ok=True)


class SoftwareAcceptanceError(RuntimeError):
    """Confirmed software check failure, distinct from an ambiguous RPC reply."""


class Release:
    def __init__(self, profile, host, component):
        self.p = profile
        if profile['host'] != host or profile['component'] != component:
            raise ValueError('profile target does not match explicit host/component')
        if not component_slug(component):
            raise ValueError('invalid component')
        self.application = component not in BUILTINS
        if self.application:
            validate_application_profile(profile)
        elif profile.get('application') or profile.get('host_lock_dir'):
            raise ValueError('builtin component cannot use application overrides')
        self.source = absolute(profile['source_dir'])
        self.root = absolute(profile['release_dir'])
        self.entry = absolute(profile['executable'])
        self.runtime = absolute(profile['runtime_dir'])
        if within(self.source, self.runtime) or within(self.runtime, self.source):
            raise ValueError('build source must be isolated from runtime')
        if within(self.root, self.runtime) or within(self.root, self.source) or within(self.runtime, self.root) or within(self.source, self.root):
            raise ValueError('release artifacts must be outside source and runtime')
        self.databases = [absolute(v) for v in profile.get('databases', [])]
        if component == 'manager' and not self.databases:
            raise ValueError('manager release requires its SQLite database backup target')
        self.configs = [absolute(v) for v in profile.get('config_files', [])]
        for value in profile.get('config_dirs', []):
            directory = absolute(value)
            if not within(directory, self.runtime) or directory.is_symlink() or not directory.is_dir():
                raise ValueError('config directory must be a real directory inside runtime')
            for file in sorted(directory.rglob('*')):
                if file.is_symlink():
                    raise ValueError('config backup refuses symlinks')
                if file.is_file() and not any(part.startswith('.') for part in file.relative_to(directory).parts):
                    self.configs.append(file)
        self.configs = list(dict.fromkeys(self.configs))
        for p in self.databases + self.configs:
            if not within(p, self.runtime):
                raise ValueError('backup file is outside the named runtime directory')
        control = profile.get('control', {})
        if component == 'communicate' and (self.entry.name != 'rcd_communicate' or
                (control and (control.get('kind') not in ('systemd-user', 'systemd-system') or
                              control.get('unit') != 'rcd-communicate.service'))):
            raise ValueError('communicate must bind rcd_communicate and its independent rcd-communicate.service')
        if control.get('run_as') and (control.get('kind') != 'systemd-user' or
                not re.fullmatch(r'[A-Za-z_][A-Za-z0-9_-]{0,31}', control['run_as'])):
            raise ValueError('run_as requires an explicit systemd user account')
        if control.get('kind') in ('systemd-user', 'systemd-system') and not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.@-]*\.service', control.get('unit', '')):
            raise ValueError('one explicit systemd service unit is required')
        for url in [control.get('url'), profile.get('health_url')]:
            if url:
                parsed = urllib.parse.urlparse(url)
                if (parsed.scheme not in ('http', 'https') or parsed.username or parsed.password or
                        parsed.query or parsed.fragment or parsed.hostname not in {'127.0.0.1', 'localhost', host}):
                    raise ValueError('control and health endpoints must belong to the explicitly named host without credentials/query')

    def local_host(self):
        if platform.system() != 'Linux' or self.p['host'] not in run('hostname', '-I').split():
            raise RuntimeError('run this tool ON the explicitly named Linux host')

    def json_request(self, suffix, payload=None):
        c = self.p['control']
        headers = {'X-RCD-Instance-ID': c['instance_id']}
        data = None
        if payload is not None:
            headers.update({'Content-Type': 'application/json', 'If-Match': '"' + payload['expected_revision'] + '"'})
            data = json.dumps(payload).encode()
        request = urllib.request.Request(c['url'].rstrip('/') + suffix, data=data, headers=headers)
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, req, fp, code, msg, headers, newurl):
                raise RuntimeError('process agent redirect refused')
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
        with opener.open(request, timeout=30) as response:
            data = response.read((1 << 20) + 1)
            if len(data) > 1 << 20:
                raise RuntimeError('process agent response exceeds limit')
            return json.loads(data)

    def systemctl(self, *arguments):
        c = self.p['control']
        scope = ['--user'] if c['kind'] == 'systemd-user' else []
        prefix = []
        if c.get('run_as'):
            import pwd
            uid = pwd.getpwnam(c['run_as']).pw_uid
            if os.geteuid() != uid:
                if os.geteuid() != 0:
                    raise RuntimeError('cross-user systemd control requires root')
                prefix = ['runuser', '-u', c['run_as'], '--', 'env',
                          'XDG_RUNTIME_DIR=/run/user/' + str(uid),
                          'DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/' + str(uid) + '/bus']
        return run(*prefix, 'systemctl', *scope, *arguments)

    def pid(self):
        c = self.p['control']
        if c['kind'] in ('systemd-user', 'systemd-system'):
            return int(self.systemctl('show', c['unit'], '-p', 'MainPID', '--value'))
        if c['kind'] != 'process-agent':
            raise ValueError('unsupported control kind')
        state = self.json_request('/api/v1/process')
        if (state.get('instance_id') != c['instance_id'] or Path(state['executable']) != self.entry or
                Path(state['work_dir']) != Path(self.p.get('work_dir', self.runtime)) or state.get('pending_request_id')):
            raise RuntimeError('process agent target mismatch or pending action')
        return int(state.get('pid') or 0)

    def control(self, action):
        c = self.p['control']
        if c['kind'] in ('systemd-user', 'systemd-system'):
            if self.application and action == 'stop':
                self.require_application_service()
                self.systemctl('stop', '--no-block', c['unit'])
                end=time.monotonic()+40
                while True:
                    output=self.systemctl('show',c['unit'],'--property=LoadState,MainPID,ActiveState,SubState,Job')
                    state=dict(line.split('=',1) for line in output.splitlines() if '=' in line)
                    if (state.get('LoadState')=='loaded' and state.get('MainPID')=='0'
                            and state.get('ActiveState') in ('inactive','failed')
                            and state.get('SubState') in ('dead','failed') and state.get('Job') in ('','0')):
                        break
                    if time.monotonic()>=end:
                        raise RuntimeError('registered application normal exit unconfirmed; no forced kill or retry')
                    time.sleep(.25)
            else:
                self.systemctl(action, c['unit'])
        else:
            state = self.json_request('/api/v1/process')
            request = {'request_id': 'release-' + uuid.uuid4().hex, 'instance_id': c['instance_id'], 'action': action, 'expected_revision': state['revision']}
            result = self.json_request('/api/v1/process/actions', request)
            end = time.monotonic() + 120
            while result['state'] not in ('succeeded', 'failed') and time.monotonic() < end:
                time.sleep(0.5)
                result = self.json_request('/api/v1/process/operations/' + request['request_id'])
            if result['state'] != 'succeeded':
                raise RuntimeError('process control failed or ambiguous; inspect before retry: ' + request['request_id'])
        if action == 'stop' and self.pid() != 0:
            raise RuntimeError('process did not stop; will not replace executable/database')

    def preflight(self):
        self.local_host()
        if not self.entry.is_file() or not self.runtime.is_dir():
            raise RuntimeError('missing runtime or executable')
        self.validate_state_inventory()
        for file in self.databases + self.configs:
            if not file.is_file():
                raise RuntimeError('missing backup target: ' + str(file))
        if self.p['component'] == 'communicate':
            self.require_stopped_communicate()
        if self.application:
            self.require_application_service()
        pid = self.pid()
        if pid and Path('/proc/' + str(pid) + '/exe').resolve() != self.entry.resolve():
            raise RuntimeError('running executable does not match target')
        if self.p['component'] == 'communicate':
            if pid:
                raise RuntimeError('通信程序尚不支持可信的活动通信排空；请由维护人员受控停止后再升级，发布器不会自动停止它')
        return {'host': self.p['host'], 'component': self.p['component'], 'pid': pid, 'binary_sha256': sha(self.entry)}

    def require_application_service(self):
        validate_application_profile(self.p)
        output = self.systemctl('show', self.p['control']['unit'], '--property=LoadState,MainPID,Job,SendSIGKILL,KillSignal,KillMode,Restart,TimeoutStopUSec,WorkingDirectory,ExecStart,FragmentPath,DropInPaths')
        values = dict(line.split('=', 1) for line in output.splitlines() if '=' in line)
        # No strong kill or restart-loop is allowed to bypass a slow normal
        # exit. This is process-level shutdown, not device transaction draining.
        if (values.get('LoadState') != 'loaded' or values.get('Job') not in ('', '0')
                or values.get('SendSIGKILL') != 'no' or values.get('KillSignal') not in ('15', 'SIGTERM')
                or values.get('KillMode') != 'control-group' or values.get('Restart') != 'no'
                or values.get('TimeoutStopUSec') not in ('30s', '30000000us')
                or values.get('WorkingDirectory') != str(self.runtime)
                or ('path=' + str(self.entry) + ' ;') not in values.get('ExecStart', '')):
            raise RuntimeError('registered application unit lacks fixed normal-stop protections; no process stopped or replaced')
        fragment = Path(values.get('FragmentPath', ''))
        if (not fragment.is_absolute() or fragment.is_symlink() or not fragment.is_file()
                or fragment.stat().st_size > 16384 or fragment.resolve() != fragment
                or values.get('DropInPaths') or fragment.name != self.p['control']['unit']
                or fragment.read_text() != unit_text(str(self.entry), str(self.runtime), self.p['application']['arguments'])):
            raise RuntimeError('registered application unit changed; refusing arbitrary argv, hooks or drop-ins')
        return values

    def require_stopped_communicate(self):
        if self.p.get('control', {}).get('kind') not in ('systemd-user', 'systemd-system'):
            raise RuntimeError('communicate must use its own fixed systemd service, never the lower Agent')
        output = self.systemctl('show', self.p['control']['unit'], '--property=LoadState,ActiveState,SubState,MainPID,Job')
        values = dict(line.split('=', 1) for line in output.splitlines() if '=' in line)
        if not (values.get('LoadState') == 'loaded' and values.get('MainPID') == '0'
                and values.get('ActiveState') in ('inactive', 'failed')
                and values.get('SubState') in ('dead', 'failed') and values.get('Job') in ('', '0')):
            raise RuntimeError('通信程序尚不支持可信的活动通信排空；服务未确认停止，发布器不会自动停止或重启它')

    def validate_state_inventory(self):
        # A maintenance generation is control state, not disposable cache.
        # Supported layouts are legacy state-at-runtime and explicit state/.
        if (self.p['component'] == 'communicate' or self.application) and self.databases:
            raise RuntimeError('communicate has no supported database format; review the profile instead of ignoring state')
        for base in (self.runtime, self.runtime / 'state'):
            for name in ('command-journal.sqlite', 'maintenance.sqlite'):
                file = base / name
                if file.exists() and (not file.is_file() or file.is_symlink() or file not in self.databases):
                    raise RuntimeError('required state database missing from backup inventory: ' + str(file))
        if self.p['component'] == 'agent':
            state = self.runtime / 'process-state.json'
            if not state.is_file() or state.is_symlink() or state not in self.configs:
                raise RuntimeError('agent process-state.json must be explicitly backed up')

    def build(self, commit, record_dirty, release_id=None, version=None):
        if self.application:
            raise ValueError('registered external software must use the binary-only package builder; no arbitrary build commands')
        self.local_host()
        if not self.source.is_dir():
            raise RuntimeError('build source directory is missing')
        if not record_dirty:
            raise RuntimeError('snapshot builds require --record-dirty; all inputs will be hashed')
        if len(commit) != 40 or any(c not in '0123456789abcdef' for c in commit):
            raise ValueError('explicit base commit must be a full lowercase SHA')
        try:
            git_revision = run('git', '-C', str(self.source), 'rev-parse', 'HEAD')
            git_status = run('git', '-C', str(self.source), 'status', '--short')
        except subprocess.CalledProcessError:
            git_revision, git_status = None, 'archive snapshot; no Git metadata'
        if git_revision and git_revision != commit:
            raise ValueError('declared base commit does not match source checkout')
        before = source_manifest(self.source)
        directory = self.root / ('build-' + uuid.uuid4().hex)
        identity = build_identity(commit, before['sha256'], release_id or directory.name,
                                  version or '0.0.0-snapshot.' + before['sha256'][:12])
        directory.mkdir(parents=True, mode=0o700)
        artifact = directory / 'binary'
        if self.p['component'] in ('manager', 'agent'):
            require_manager_web_sources(self.source)
            run('npm', 'ci', cwd=self.source / 'manager/web')
            # Release builds share the industrial test host with running OPC
            # services. Bound jsdom workers rather than consuming every CPU.
            run('npm', 'test', '--', '--maxWorkers=2', '--minWorkers=1', cwd=self.source / 'manager/web')
            run('npm', 'run', 'build', cwd=self.source / 'manager/web')
            test_env = os.environ.copy()
            test_env['RCD_VERIFY_WEB_HELP'] = '1'
            run('go', 'test', './...', cwd=self.source / 'manager', env=test_env)
            run('go', 'build', '-trimpath', '-ldflags', identity_ldflags(identity), '-o', str(artifact), './cmd/rcd-manager', cwd=self.source / 'manager')
        elif self.p['component'] == 'communicate':
            project = self.source / 'communicate'
            run('cargo', 'test', '--locked', '-j2', cwd=project)
            run('cargo', 'build', '--locked', '--release', '-j2', cwd=project, env=identity_environment(identity))
            target = Path(os.environ.get('CARGO_TARGET_DIR', project / 'target'))
            shutil.copy2(target / 'release/rcd_communicate', artifact)
        else:
            run('cargo', 'test', '--locked', '--lib', '-j2', cwd=self.source)
            run('cargo', 'build', '--locked', '--release', '-j2', cwd=self.source, env=identity_environment(identity))
            target = Path(os.environ.get('CARGO_TARGET_DIR', self.source / 'target'))
            shutil.copy2(target / 'release/rcd_server', artifact)
        if before != source_manifest(self.source):
            raise RuntimeError('source/dependencies changed during build; do not publish this artifact')
        version_args = ['agent', 'version', '--json'] if self.p['component'] == 'agent' else ['version', '--json']
        reported = json.loads(run(str(artifact), *version_args, cwd=directory))
        if reported.get('component') != self.p['component'] or any(reported.get(k) != v for k, v in identity.items()):
            raise RuntimeError('binary build identity does not match source manifest; do not publish')
        manifest = {'host': self.p['host'], 'component': self.p['component'], 'declared_base_commit': commit,
                    'build_info': reported,
                    'git_revision': git_revision, 'git_status': git_status,
                    'dirty_snapshot_recorded': True, 'source': before, 'binary_sha256': sha(artifact),
                    'built_at': time.time(), 'toolchain': run('rustc', '--version') if self.p['component'] in ('lower', 'communicate') else run('go', 'version')}
        (directory / 'manifest.json').write_text(json.dumps(manifest, indent=2))
        return {'release': str(directory), 'sha256': manifest['binary_sha256']}

    def verify(self, directory):
        directory = absolute(str(directory))
        if not within(directory, self.root):
            raise ValueError('artifact is outside the declared release directory')
        m = json.loads((directory / 'manifest.json').read_text())
        if m['host'] != self.p['host'] or m['component'] != self.p['component'] or m['binary_sha256'] != sha(directory / 'binary'):
            raise RuntimeError('artifact target or SHA-256 mismatch')
        if 'staging' in m:
            from orchestration import verify_staging
            verify_staging(self, directory, m)
        return m

    def backup(self, directory):
        self.validate_state_inventory()
        backup = directory / ('backup-' + uuid.uuid4().hex)
        backup.mkdir(mode=0o700)
        shutil.copy2(self.entry, backup / 'binary')
        record = {'host': self.p['host'], 'component': self.p['component'], 'executable': str(self.entry),
                  'binary_sha256': sha(backup / 'binary'), 'link': os.readlink(self.entry) if self.entry.is_symlink() else None, 'files': []}
        for i, file in enumerate(self.databases + self.configs):
            stored = backup / ('file-' + str(i))
            database = file in self.databases
            schema = sqlite_backup(file, stored) if database else None
            if not database:
                shutil.copy2(file, stored)
            record['files'].append({'target': str(file), 'file': stored.name, 'database': database, 'schema_version': schema, 'sha256': sha(stored)})
        for file in backup.iterdir():
            with file.open('rb') as stream:
                os.fsync(stream.fileno())
        durable_json(backup / 'backup.json', record)
        sync_directory(directory)
        return backup

    def accept(self, expected):
        end = time.monotonic() + 60
        while time.monotonic() < end:
            pid = self.pid()
            try:
                matching_image = pid and sha('/proc/' + str(pid) + '/exe') == expected
            except (FileNotFoundError, ProcessLookupError):
                matching_image = False
            if matching_image:
                try:
                    if self.application:
                        # Generic software has no prescribed HTTP/OPC protocol.
                        # Verify an actual stable process image, not disk claims.
                        time.sleep(1)
                        if self.pid() == pid and sha('/proc/' + str(pid) + '/exe') == expected:
                            return {'pid': pid, 'binary_sha256': expected, 'normal_process_ready': True}
                        continue
                    if self.p.get('health_url'):
                        with urllib.request.urlopen(self.p['health_url'], timeout=3) as response:
                            if not json.load(response).get('ok'):
                                raise RuntimeError('not ready')
                    if self.p['component'] == 'agent':
                        # Agent health is independent of whether its child was
                        # intentionally stopped. The transaction validates the
                        # child's original state and maintenance separately.
                        return {'pid': pid, 'binary_sha256': expected, 'agent_http_ready': True}
                    with socket.create_connection(('127.0.0.1', int(self.p['opc_port'])), timeout=3):
                        return {'pid': pid, 'binary_sha256': expected, 'opc_tcp_reachable': True}
                except (OSError, ValueError, RuntimeError):
                    pass
            time.sleep(0.5)
        raise SoftwareAcceptanceError('acceptance failed; retain evidence and explicitly request rollback')

    def deploy(self, directory):
        with target_lock(self):
            require_no_unfinished_release(self)
            return self._deploy_manual(directory)

    def _deploy_manual(self, directory):
        self.guard_agent_maintenance_state()
        self.preflight()
        manifest = self.verify(directory)
        self.control('stop')
        backup = self.backup(directory)
        # Print recovery location before replacing anything; failures do not
        # silently resume a possibly migrated binary with an old database.
        print(json.dumps({'recovery_backup': str(backup)}), flush=True)
        atomic_copy(directory / 'binary', self.entry)
        self.entry.chmod(0o755)
        self.control('start')
        result = self.accept(manifest['binary_sha256'])
        (backup / 'acceptance.json').write_text(json.dumps(result, indent=2))
        return result

    def rollback(self, backup, restore_data=False):
        with target_lock(self):
            require_no_unfinished_release(self)
            return self._rollback_manual(backup, restore_data)

    def _rollback_manual(self, backup, restore_data=False):
        self.local_host()
        self.validate_state_inventory()
        # This legacy recovery path cannot establish compatibility with a
        # schema-v2 agent nor restore monotonic generations. Refuse before stop.
        self.guard_agent_maintenance_state()
        if restore_data and any(p.name == 'maintenance.sqlite' for p in self.databases):
            raise RuntimeError('maintenance generation cannot be restored from an old backup')
        if not within(backup, self.root):
            raise ValueError('backup is outside release directory')
        record = json.loads((backup / 'backup.json').read_text())
        if record['host'] != self.p['host'] or record['component'] != self.p['component'] or record['executable'] != str(self.entry):
            raise ValueError('backup target mismatch')
        if sha(backup / 'binary') != record['binary_sha256']:
            raise ValueError('backup binary damaged')
        allowed = {str(p) for p in self.databases + self.configs}
        if {f['target'] for f in record['files']} != allowed:
            raise ValueError('backup does not cover the complete current profile')
        for f in record['files']:
            if f['target'] not in allowed or f['database'] != (Path(f['target']) in self.databases) or Path(f['file']).name != f['file'] or sha(backup / f['file']) != f['sha256']:
                raise ValueError('backup file target or digest mismatch')
        self.control('stop')
        # Always preserve current user data. A binary-only rollback must not
        # silently discard edits made since deployment.
        preserved = self.backup(backup.parent)
        print(json.dumps({'pre_rollback_backup': str(preserved)}), flush=True)
        if not restore_data:
            current = json.loads((preserved / 'backup.json').read_text())
            current_schemas = {f['target']: f['schema_version'] for f in current['files'] if f['database']}
            if any(current_schemas[f['target']] != f['schema_version'] for f in record['files'] if f['database']):
                raise RuntimeError('database schema changed; stopped safely with current data preserved. '
                                   'Explicit data-restore authorization is required; do not retry automatically')
        else:
            for f in record['files']:
                if f['database']:
                    sqlite_backup(backup / f['file'], Path(f['target']))
                else:
                    atomic_copy(backup / f['file'], Path(f['target']))
        atomic_copy(backup / 'binary', self.entry)
        if record.get('link'):
            target = (self.entry.parent / record['link']).resolve()
            if target.is_file() and sha(target) == record['binary_sha256']:
                temporary = self.entry.with_name(self.entry.name + '.release-' + uuid.uuid4().hex)
                temporary.symlink_to(record['link'])
                os.replace(temporary, self.entry)
        self.control('start')
        return self.accept(record['binary_sha256'])

    def guard_agent_maintenance_state(self):
        if self.application:
            raise RuntimeError('registered applications require the native controlled publisher')
        if self.p['component'] == 'communicate':
            raise RuntimeError('communicate requires the controlled single-component publisher; legacy manual restart/rollback is unsupported')
        if self.p['component'] == 'agent':
            self.validate_state_inventory()
            state = json.loads((self.runtime / 'process-state.json').read_text())
            if state.get('schema_version') != 1 or state.get('maintenance') is not None:
                raise RuntimeError('agent maintenance state requires a separate compatibility/recovery plan')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['preflight', 'build', 'verify', 'deploy', 'rollback', 'inspect-deployment', 'finish-deployment', 'resume-deployment', 'rollback-deployment', 'resume-rollback', 'finish-rollback'])
    parser.add_argument('--profile', required=True)
    parser.add_argument('--host', required=True)
    parser.add_argument('--component', required=True)
    parser.add_argument('--artifact', type=Path)
    parser.add_argument('--base-commit', default='')
    parser.add_argument('--record-dirty', action='store_true')
    parser.add_argument('--release-id', help='shared identity for this release set')
    parser.add_argument('--app-version', help='application version injected into the binary')
    parser.add_argument('--maintenance', action='store_true', help='use durable lower/agent fenced deployment')
    parser.add_argument('--deployment-id', help='unique durable deployment identity; never reused')
    parser.add_argument('--initiator', default='', help='operator audit label, not authentication')
    parser.add_argument('--confirm-live-target', help='deployment authorization: exact host/component')
    parser.add_argument('--confirm-no-active-commands', action='store_true', help='operator has established a control maintenance window')
    parser.add_argument('--confirm-restore-backup-data', action='store_true', help='separate authorization to overwrite current configuration and database with the selected backup')
    args = parser.parse_args()
    release = Release(json.loads(Path(args.profile).read_text()), args.host, args.component)
    release.local_host()
    if args.maintenance and args.action != 'deploy':
        parser.error('--maintenance is only supported for deploy; rollback requires separate recovery review')
    if args.confirm_restore_backup_data and args.action != 'rollback':
        parser.error('--confirm-restore-backup-data is only valid with rollback')
    if (args.maintenance or args.action in ('inspect-deployment', 'finish-deployment', 'resume-deployment', 'rollback-deployment', 'resume-rollback', 'finish-rollback')) and not args.deployment_id:
        parser.error('--deployment-id required')
    if args.action in ('deploy', 'rollback', 'finish-deployment', 'resume-deployment', 'rollback-deployment', 'resume-rollback', 'finish-rollback') and (args.confirm_live_target != args.host + '/' + args.component or not args.confirm_no_active_commands):
        parser.error('live actions require exact target confirmation AND a no-active-commands maintenance window')
    if args.action == 'inspect-deployment':
        result = Deployment(release, args.deployment_id).inspect()
    elif args.action == 'finish-deployment':
        from recovery import FinishDeployment
        result = FinishDeployment(release, args.deployment_id).finish(args.initiator)
    elif args.action == 'resume-deployment':
        from resume import ResumeStoppedDeployment
        result = ResumeStoppedDeployment(release, args.deployment_id).resume(args.initiator)
    elif args.action in ('rollback-deployment', 'resume-rollback', 'finish-rollback'):
        from rollback_deployment import RollbackBeforeStart
        operation = RollbackBeforeStart(release, args.deployment_id)
        action = {'rollback-deployment': operation.rollback, 'resume-rollback': operation.resume_rollback, 'finish-rollback': operation.finish}[args.action]
        result = action(args.initiator)
    elif args.action == 'preflight':
        result = release.preflight()
    elif args.action == 'build':
        result = release.build(args.base_commit, args.record_dirty, args.release_id, args.app_version)
    else:
        if args.artifact is None:
            parser.error('--artifact required')
        if args.confirm_restore_backup_data and args.action != 'rollback':
            parser.error('--confirm-restore-backup-data is only valid with rollback')
        if args.maintenance:
            result = Deployment(release, args.deployment_id).deploy(args.artifact, args.initiator)
        else:
            result = release.rollback(args.artifact, restore_data=args.confirm_restore_backup_data) if args.action == 'rollback' else getattr(release, args.action)(args.artifact)
    print(json.dumps(result, ensure_ascii=False))


if __name__ == '__main__':
    main()
