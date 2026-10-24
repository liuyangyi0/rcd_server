#!/usr/bin/env python3
"""Native Semaphore host onboarding probes and no-clobber first installation.

Only trusted queue bindings select paths. No shell, package scripts, binary
execution, business configuration generation, enable/start or serial access.
"""
import argparse
import base64
import fcntl
from contextlib import contextmanager
from datetime import datetime, timezone
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import platform
import pwd
import re
import shutil
import stat
import subprocess
import tarfile
import tempfile
from types import SimpleNamespace

from application_profile import application_slug, component_slug, validate_application_profile, unit_text
from deployment import durable_json, require_no_unfinished_release, target_lock
from release_probe import ADAPTER_VERSION

ACCOUNT = re.compile(r'[A-Za-z_][A-Za-z0-9_-]{0,31}\Z')
HASH = re.compile(r'[a-f0-9]{64}\Z')
BUILTIN_ENTRY = {'lower': 'rcd_server', 'manager': 'RcdManager', 'agent': 'RcdAgent', 'communicate': 'rcd_communicate'}


class InstallNotStarted(RuntimeError):
    """A checked rejection before publishing any installation intent."""


def now():
    return datetime.now(timezone.utc).isoformat().replace('+00:00', 'Z')


def read_json(path, limit=65536):
    path = Path(path)
    if path.is_symlink() or not path.is_file() or path.stat().st_size > limit:
        raise ValueError('regular bounded JSON required')
    def unique(items):
        result = {}
        for key, value in items:
            if key in result:
                raise ValueError('duplicate JSON field')
            result[key] = value
        return result
    with path.open(encoding='utf-8') as stream:
        return json.load(stream, object_pairs_hook=unique)


def digest(path):
    h = hashlib.sha256()
    with Path(path).open('rb') as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b''):
            h.update(block)
    return h.hexdigest()


def request_identity(req):
    if (not isinstance(req, dict) or req.get('schema_version') != 1
            or not HASH.fullmatch(req.get('operation_id', ''))
            or type(req.get('task_id')) is not int or req['task_id'] < 1
            or type(req.get('project_id')) is not int or req['project_id'] < 1
            or not ACCOUNT.fullmatch(req.get('service_account', '')) or req['service_account'] == 'root'):
        raise ValueError('server-bound host setup identity required')
    if 'admission_id' in req and (not isinstance(req['admission_id'], str) or not HASH.fullmatch(req['admission_id'])):
        raise ValueError('invalid installation admission identity')
    address = str(ipaddress.ip_address(req.get('address', '')))
    port = req.get('ssh_port')
    if type(port) is not int or not 1 <= port <= 65535:
        raise ValueError('invalid SSH port')
    return address, port


def publish_first_identity(req, plan, binary, manifest):
    """Public, hash-backed identity, not a fake running-version observation."""
    root = Path('/var/lib/rcd-release-platform/artifacts') / req['component']
    ensure_directory(root, 0o755)
    release = root / ('first-install-' + req['operation_id'])
    if release.exists() or release.is_symlink():
        raise RuntimeError('first-install identity directory already exists; inspect original receipt')
    ensure_directory(release, 0o755)
    name = plan['entry'].name
    new_binary(release / name, binary)
    fields = ('schema_version', 'component', 'version', 'release_id', 'commit', 'built_at', 'source_sha256', 'dirty', 'target')
    identity = {'components': {name: {'sha256': req['binary_sha256'], 'build_info': {key: manifest[key] for key in fields}}}}
    new_file(release / 'identity-public.json', json.dumps(identity, sort_keys=True).encode(), 0o644)
    return str(release)


def base_report(req, phase):
    request_identity(req)
    report = {'schema_version': 1, 'operation_id': req['operation_id'], 'task_id': req['task_id'],
            'project_id': req['project_id'], 'phase': phase, 'observed_at': now(),
            'state': 'failed', 'error': ''}
    if 'admission_id' in req:
        report['admission_id'] = req['admission_id']
    for key in ('component', 'package_id', 'archive_sha256', 'binary_sha256'):
        if key in req:
            report[key] = req[key]
    return report


def keyscan(req, run=subprocess.run):
    report = base_report(req, 'keyscan')
    address, port = request_identity(req)
    executable = next((p for p in ('/usr/bin/ssh-keyscan', '/bin/ssh-keyscan') if Path(p).is_file()), None)
    if executable is None:
        raise RuntimeError('controller prerequisite missing: ssh-keyscan')
    result = run([executable, '-T', '5', '-p', str(port), '-t', 'ed25519,ecdsa,rsa', address],
                 stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                 text=True, timeout=20, check=False)
    if len(result.stdout) > 65536:
        raise ValueError('oversized host key response')
    keys = {}
    expected_host = address if port == 22 else '[' + address + ']:' + str(port)
    for line in result.stdout.splitlines():
        if not line or line.startswith('#'):
            continue
        fields = line.split()
        if len(fields) != 3 or fields[0] != expected_host or fields[1] not in ('ssh-ed25519', 'ecdsa-sha2-nistp256', 'ecdsa-sha2-nistp384', 'ecdsa-sha2-nistp521', 'ssh-rsa'):
            raise ValueError('invalid host keyscan result')
        raw = base64.b64decode(fields[2], validate=True)
        if len(raw) > 8192 or len(raw) < 16:
            raise ValueError('invalid SSH public host key')
        fingerprint = 'SHA256:' + base64.b64encode(hashlib.sha256(raw).digest()).decode().rstrip('=')
        key = {'algorithm': fields[1], 'fingerprint': fingerprint, 'public_key': fields[1] + ' ' + fields[2]}
        if fields[1] in keys and keys[fields[1]] != key:
            raise ValueError('conflicting host keys returned')
        keys[fields[1]] = key
    if not keys:
        raise RuntimeError('SSH host keyscan returned no keys; no credentials were sent')
    report.update(state='awaiting_host_key', host_keys=sorted(keys.values(), key=lambda k: k['algorithm']))
    return report


def probe(req, run=subprocess.run):
    report = base_report(req, 'probe')
    if platform.system() != 'Linux':
        raise RuntimeError('only Linux hosts are supported')
    account = pwd.getpwnam(req['service_account'])
    if account.pw_uid == 0 or not Path(account.pw_dir).is_absolute() or Path(account.pw_dir).is_symlink():
        raise RuntimeError('an existing non-root service account with a regular home is required')
    machine = platform.machine()
    target = {'x86_64': 'linux/amd64', 'aarch64': 'linux/arm64'}.get(machine)
    if not target:
        raise RuntimeError('unsupported Linux CPU architecture')
    if not Path('/run/systemd/system').is_dir():
        raise RuntimeError('systemd host required; no services were changed')
    # This only checks prerequisites. It neither enables linger nor creates a
    # login session, account, package manager dependency or business process.
    if not Path('/usr/bin/python3').is_file():
        raise RuntimeError('target prerequisite missing: /usr/bin/python3')
    report.update(state='ready_for_install', platform=target, service_uid=account.pw_uid,
                  service_home=account.pw_dir, installed=False, business_ready=False)
    return report


def regular_parents(path):
    path = Path(path)
    if not path.is_absolute() or '..' in path.parts:
        raise ValueError('fixed absolute path required')
    for parent in [*reversed(path.parents), path]:
        if parent.exists() or parent.is_symlink():
            info = parent.lstat()
            if not stat.S_ISDIR(info.st_mode):
                raise RuntimeError('unsafe or symlink directory in install path')


def require_system_directory(descriptor):
    info = os.fstat(descriptor)
    if info.st_uid != 0 or info.st_mode & 0o022:
        raise RuntimeError('controlled system directory must be root-owned and not group/world writable; refusing adoption')


def ensure_directory(path, mode=0o755, owner=None):
    path = Path(path)
    if not path.is_absolute() or '..' in path.parts:
        raise ValueError('fixed absolute directory required')
    descriptor = os.open('/', os.O_RDONLY | os.O_DIRECTORY)
    try:
        if owner is None:
            require_system_directory(descriptor)
        for part in path.parts[1:]:
            created = False
            try:
                next_fd = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=descriptor)
            except FileNotFoundError:
                os.mkdir(part, mode, dir_fd=descriptor)
                os.fsync(descriptor)
                next_fd = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=descriptor)
                created = True
            os.close(descriptor)
            descriptor = next_fd
            if owner is None:
                # Refuse unsafe EXISTING directories; never repair their
                # ownership or permissions as a side effect of installation.
                require_system_directory(descriptor)
            if created and owner is not None:
                os.fchown(descriptor, owner.pw_uid, owner.pw_gid)
        if owner is not None and os.fstat(descriptor).st_uid != owner.pw_uid:
            raise RuntimeError('existing data/home directory has a different owner; refusing adoption')
    finally:
        os.close(descriptor)


def new_file(path, data, mode, owner=None):
    path = Path(path)
    directory_fd = open_directory(path.parent)
    try:
        fd = os.open(path.name, os.O_CREAT | os.O_EXCL | os.O_WRONLY | os.O_NOFOLLOW, mode, dir_fd=directory_fd)
        try:
            os.fchmod(fd, mode)
            if owner is not None:
                os.fchown(fd, owner.pw_uid, owner.pw_gid)
            with os.fdopen(fd, 'wb', closefd=False) as stream:
                stream.write(data)
                stream.flush()
                os.fsync(fd)
        finally:
            os.close(fd)
        os.fsync(directory_fd)
    finally:
        os.close(directory_fd)


def new_binary(path, source):
    """Publish with O_EXCL and bounded memory; never replace an old entry."""
    path = Path(path)
    directory_fd = open_directory(path.parent)
    try:
        fd = os.open(path.name, os.O_CREAT | os.O_EXCL | os.O_WRONLY | os.O_NOFOLLOW, 0o755, dir_fd=directory_fd)
        try:
            os.fchmod(fd, 0o755)
            with Path(source).open('rb') as stream, os.fdopen(fd, 'wb', closefd=False) as output:
                shutil.copyfileobj(stream, output, 1024 * 1024)
                output.flush()
                os.fsync(fd)
        finally:
            os.close(fd)
        os.fsync(directory_fd)
    finally:
        os.close(directory_fd)


def open_directory(path):
    """Pin every ancestor with openat/O_NOFOLLOW, including user-owned homes."""
    path = Path(path)
    if not path.is_absolute() or '..' in path.parts:
        raise ValueError('absolute directory required')
    descriptor = os.open('/', os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in path.parts[1:]:
            next_fd = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=descriptor)
            os.close(descriptor)
            descriptor = next_fd
        return descriptor
    except BaseException:
        os.close(descriptor)
        raise


def fixed_plan(req, account):
    component = req.get('component')
    if not component_slug(component):
        raise ValueError('unsupported component')
    if component in BUILTIN_ENTRY:
        return {'entry': Path('/opt/rcd/bin') / BUILTIN_ENTRY[component], 'component': component,
                'runtime': None, 'unit': None, 'profile': None, 'owner_marker': None,
                'warning': '内置 RCD 二进制已安装；缺少经核验的业务配置/维护绑定，保持停止，不能当作已接入可运行。'}
    app = req.get('application')
    if (not isinstance(app, dict) or app.get('component') != component
            or app.get('install_path') != '/opt/rcd/apps/' + component + '/bin/' + component
            or app.get('work_dir') != '/var/lib/rcd/apps/' + component
            or app.get('service_unit') != 'rcd-app-' + component + '.service'
            or app.get('service_user') != 'device_owner'
            or app.get('version_mode') != 'manifest_sha256'):
        raise ValueError('immutable registered application definition required')
    marker = {key: app.get(key) for key in ('component', 'definition_sha256', 'arguments', 'graceful_shutdown_confirmed')}
    marker.update(schema_version=1, project_id=req['project_id'])
    profile = {'component': component, 'executable': app['install_path'], 'runtime_dir': app['work_dir'],
               'work_dir': app['work_dir'], 'host_lock_dir': '/opt/rcd/bin', 'databases': [],
               'control': {'kind': 'systemd-user', 'unit': app['service_unit'], 'run_as': account.pw_name},
               'application': marker, 'source_dir': '/var/lib/rcd-release-platform/sources/' + component,
               'release_dir': '/var/lib/rcd-release-platform/artifacts/' + component, 'host': req['address']}
    validate_application_profile(profile, check_owner=False)
    unit = Path(account.pw_dir) / '.config/systemd/user' / app['service_unit']
    return {'entry': Path(app['install_path']), 'component': component, 'runtime': Path(app['work_dir']),
            'unit': unit, 'profile': profile, 'profile_path': Path('/etc/rcd/release-platform') / (component + '.json'),
            'owner_marker': Path(app['work_dir']) / '.rcd-application-owner.json', 'warning': '已安装，保持停止；请先完成应用配置，再由维护人员明确启用。'}


def first_install_hold(req):
    return {'schema_version': 1, 'desired_state': 'stopped',
            'reason': 'first_install_pending_configuration', 'operation_id': req['operation_id']}


def require_no_existing_process(plan, proc=Path('/proc')):
    """Do not turn an untracked/deleted executable into an adopted service."""
    expected = plan['entry'].name
    for directory in proc.iterdir():
        if not directory.name.isdecimal():
            continue
        try:
            image = os.readlink(directory / 'exe')
        except FileNotFoundError:
            continue  # exited or a kernel thread
        except PermissionError:
            try:
                if (directory / 'comm').read_text().strip() == expected[:15]:
                    raise RuntimeError('a possible target process cannot be inspected; refusing first install')
            except FileNotFoundError:
                pass
            continue
        path = image[:-10] if image.endswith(' (deleted)') else image
        if Path(path).name == expected:
            raise RuntimeError('an untracked or already running target exists; first install will not adopt or stop it')


def extract_verified(req, package, directory):
    package = Path(package)
    if package.is_symlink() or not package.is_file() or package.stat().st_size > 256 << 20 or digest(package) != req.get('archive_sha256'):
        raise ValueError('package archive digest mismatch')
    seen = set()
    with tarfile.open(package, 'r:gz') as archive:
        for member in archive:
            if member.name not in ('binary', 'manifest.json') or member.name in seen or not member.isfile() or member.linkname or member.pax_headers:
                raise ValueError('only distinct regular binary/manifest package members are supported')
            seen.add(member.name)
            maximum = 512 << 20 if member.name == 'binary' else 65536
            if member.size < 1 or member.size > maximum:
                raise ValueError('package member size limit')
            source = archive.extractfile(member)
            with source, (directory / member.name).open('xb') as destination:
                shutil.copyfileobj(source, destination, 1024 * 1024)
    if seen != {'binary', 'manifest.json'}:
        raise ValueError('incomplete package')
    manifest = read_json(directory / 'manifest.json')
    binary = directory / 'binary'
    if (manifest.get('component') != req.get('component') or manifest.get('binary_sha256') != req.get('binary_sha256')
            or digest(binary) != manifest.get('binary_sha256') or binary.stat().st_size != manifest.get('binary_size')):
        raise ValueError('binary package identity mismatch')
    target = {'x86_64': 'linux/amd64', 'aarch64': 'linux/arm64'}.get(platform.machine())
    aliases = {'linux/x86_64': 'linux/amd64', 'linux/aarch64': 'linux/arm64'}
    if aliases.get(manifest.get('target'), manifest.get('target')) != target:
        raise ValueError('package target architecture does not match this host')
    with binary.open('rb') as stream:
        elf = stream.read(64)
    if (len(elf) != 64 or elf[:7] != b'\x7fELF\x02\x01\x01' or int.from_bytes(elf[16:18], 'little') not in (2, 3)
            or int.from_bytes(elf[18:20], 'little') != {'linux/amd64': 62, 'linux/arm64': 183}.get(target)):
        raise ValueError('package ELF machine mismatch')
    # Dependency-bearing first installs are not silently accepted against an
    # empty host; existing installs must go through the normal release checks.
    if manifest.get('compatibility', {}).get('component_min_versions'):
        raise ValueError('first-install package has component dependencies; install and verify prerequisites first')
    minimum = manifest.get('compatibility', {}).get('adapter_min_version', '')
    if not isinstance(minimum, str) or not re.fullmatch(r'\d+\.\d+\.\d+', minimum):
        raise ValueError('explicit minimum adapter version required')
    if tuple(map(int, minimum.split('.'))) > tuple(map(int, ADAPTER_VERSION.split('.'))):
        raise ValueError('first-install adapter version is too old for this package')
    return binary, manifest


def install(req, package):
    context = {'intent': False}
    try:
        return _install(req, package, context)
    except Exception as error:
        if not context['intent']:
            raise InstallNotStarted(str(error)) from error
        raise


def evidence_exists(path, context):
    """An unreadable prior intent is unknown, never proof nothing happened."""
    try:
        Path(path).lstat()
    except FileNotFoundError:
        return False
    except OSError:
        context['intent'] = True
        raise
    return True


def existing_receipt(req, context):
    journal = Path('/var/lib/rcd-host-setup') / (req['operation_id'] + '.json')
    if not evidence_exists(journal, context):
        return None
    context['intent'] = True
    old = read_json(journal)
    if old.get('schema_version') != 1 or old.get('request') != req:
        raise RuntimeError('operation identity/content conflict; original receipt retained')
    report = old.get('report')
    expected = base_report(req, 'install')
    if (not isinstance(report, dict) or any(report.get(k) != expected[k] for k in
            ('schema_version', 'operation_id', 'task_id', 'project_id', 'phase'))
            or report.get('admission_id') != expected.get('admission_id')):
        raise RuntimeError('original installation receipt identity is invalid')
    return report


def mark_prior_intents(req, context):
    """Use the same per-marker terminal rules as deployment.py."""
    component = req.get('component')
    if not component_slug(component):
        raise ValueError('unsupported component')
    entry = (Path('/opt/rcd/bin') / BUILTIN_ENTRY[component] if component in BUILTIN_ENTRY
             else Path('/opt/rcd/apps') / component / 'bin' / component)
    markers = ((Path('/opt/rcd/bin/.rcd-host-release-active.json'), ('succeeded', 'recovered', 'failed_before_install')),
               (entry.parent / ('.' + entry.name + '.release-active.json'), ('succeeded',)),
               (Path('/opt/rcd/bin/.rcd-host-control/lifecycle.json'), ('succeeded', 'failed')))
    for marker, terminal in markers:
        if evidence_exists(marker, context):
            try:
                previous = read_json(marker)
            except (OSError, ValueError):
                context['intent'] = True
                raise
            if previous.get('state') not in terminal:
                context['intent'] = True


@contextmanager
def read_only_host_lock():
    """Serialize evidence reads without creating/changing the host lock file."""
    parent = open_directory(Path('/opt/rcd/bin'))
    descriptor = None
    try:
        descriptor = os.open('.rcd-host-release.lock', os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=parent)
        info = os.fstat(descriptor)
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
            raise RuntimeError('host release lock must be a regular non-linked file')
        fcntl.flock(descriptor, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield
    finally:
        if descriptor is not None:
            os.close(descriptor)
        os.close(parent)


def reconcile(req):
    """Read original receipt and current proof; never retry installation."""
    report = base_report(req, 'reconcile')
    original = req.get('original_task_id')
    if type(original) is not int or original < 1 or original == req['task_id']:
        raise ValueError('original native installation task required')
    original_request = dict(req, task_id=original)
    del original_request['original_task_id']
    with read_only_host_lock():
        receipt = existing_receipt(original_request, {'intent': False})
        if receipt is None or receipt.get('state') != 'installed_stopped':
            raise RuntimeError('原首装没有可确认的完成记录；保持结果未知，不重新安装')
        if receipt.get('binary_sha256') != req.get('binary_sha256'):
            raise RuntimeError('original receipt package digest mismatch')
        marker = read_json('/opt/rcd/bin/.rcd-host-release-active.json')
        if (marker.get('deployment_id') != req['operation_id'] or marker.get('kind') != 'host_first_install'
                or marker.get('state') != 'succeeded' or marker.get('component') != req.get('component')):
            raise RuntimeError('原宿主事务未完成或已变化；仅保留证据，不解除占用')
        account = pwd.getpwnam(req['service_account'])
        plan = fixed_plan(req, account)
        regular_parents(plan['entry'].parent)
        if plan['entry'].is_symlink() or not plan['entry'].is_file() or digest(plan['entry']) != req.get('binary_sha256'):
            raise RuntimeError('current binary does not match the original first-install receipt')
        require_no_existing_process(plan)
        if plan['runtime'] is not None:
            if read_json(plan['owner_marker']) != plan['profile']['application'] or read_json(plan['profile_path']) != plan['profile']:
                raise RuntimeError('application ownership/profile changed; original receipt alone is insufficient')
            if (plan['unit'].is_symlink() or plan['unit'].read_text() != unit_text(str(plan['entry']), str(plan['runtime']), plan['profile']['application']['arguments'])
                    or read_json(plan['runtime'] / 'manual-stop.json') != first_install_hold(req)):
                raise RuntimeError('application stopped/configuration guard no longer matches the first install')
        report.update(state='installed_stopped', original_task_id=original, binary_sha256=req['binary_sha256'],
                      warning=plan['warning'], original_report=receipt, installed=True, business_ready=False)
    return report


def _install(req, package, context):
    report = base_report(req, 'install')
    # Consult evidence BEFORE privilege/prerequisite checks. A restart or changed
    # account must never turn a partially applied original operation into a
    # fresh pre-install failure and release its reservation.
    previous = existing_receipt(req, context)
    if previous is not None:
        return previous
    mark_prior_intents(req, context)
    if os.geteuid() != 0:
        raise RuntimeError('first install requires explicit native sudo/root permission')
    probe(req)
    if not HASH.fullmatch(req.get('package_id', '')) or not HASH.fullmatch(req.get('binary_sha256', '')):
        raise ValueError('server-bound verified package identity required')
    account = pwd.getpwnam(req['service_account'])
    plan = fixed_plan(req, account)
    ensure_directory('/opt/rcd/bin')
    journal_dir = Path('/var/lib/rcd-host-setup')
    ensure_directory(journal_dir, 0o700)
    journal = journal_dir / (req['operation_id'] + '.json')
    lock = SimpleNamespace(entry=plan['entry'], runtime=journal_dir, p={'host_lock_dir': '/opt/rcd/bin'})
    # A single lock is shared with every old/new release and lifecycle entry.
    ensure_directory(plan['entry'].parent)
    with target_lock(lock):
        previous = existing_receipt(req, context)
        if previous is not None:
            return previous
        mark_prior_intents(req, context)
        require_no_unfinished_release(lock)
        destinations = [plan['entry']]
        if plan['unit'] is not None:
            destinations += [plan['unit'], plan['profile_path'], plan['owner_marker']]
            system_units = [Path('/etc/systemd/system') / plan['unit'].name, Path('/usr/lib/systemd/system') / plan['unit'].name]
            if any(p.exists() or p.is_symlink() for p in system_units):
                raise RuntimeError('existing system service ownership is not a first install')
        if any(p.exists() or p.is_symlink() for p in destinations):
            raise RuntimeError('existing program/unit/profile/ownership marker: refusing overwrite or adoption')
        # Business data may belong to another project even without a marker.
        if plan['runtime'] is not None and plan['runtime'].exists() and any(plan['runtime'].iterdir()):
            raise RuntimeError('existing application data: refusing adoption')
        if plan['unit'] is not None:
            enabled = plan['unit'].parent / 'default.target.wants' / plan['unit'].name
            if enabled.exists() or enabled.is_symlink():
                raise RuntimeError('an existing enablement link could auto-start this program; refusing adoption')
        require_no_existing_process(plan)
        marker = Path('/opt/rcd/bin/.rcd-host-release-active.json')
        with tempfile.TemporaryDirectory(prefix='.first-install-', dir='/opt/rcd/bin') as temporary:
            binary, manifest = extract_verified(req, package, Path(temporary))
            record = {'schema_version': 1, 'request': req, 'report': dict(report, state='unknown', error='首装进行中；重启后只补查原记录，不自动重投')}
            context['intent'] = True  # set before the first possibly partial write
            durable_json(journal, record)
            durable_json(marker, {'schema_version': 1, 'deployment_id': req['operation_id'], 'state': 'running', 'component': req['component'], 'kind': 'host_first_install'})
            try:
                if plan['runtime'] is not None:
                    ensure_directory(plan['runtime'].parent)
                    ensure_directory(plan['runtime'], 0o750, account)
                    ensure_directory(plan['unit'].parent, 0o700, account)
                    ensure_directory(plan['profile_path'].parent, 0o755)
                    new_file(plan['owner_marker'], json.dumps(plan['profile']['application'], sort_keys=True).encode(), 0o600, account)
                    new_file(plan['profile_path'], json.dumps(plan['profile'], sort_keys=True).encode(), 0o600)
                    new_file(plan['unit'], unit_text(str(plan['entry']), str(plan['runtime']), plan['profile']['application']['arguments']).encode(), 0o600, account)
                    new_file(plan['runtime'] / 'manual-stop.json', json.dumps(first_install_hold(req)).encode(), 0o600, account)
                # Program is copied only after the validated immutable receipt
                # and path ownership. There is no startup or config injection.
                new_binary(plan['entry'], binary)
                if digest(plan['entry']) != req['binary_sha256']:
                    raise RuntimeError('installed binary digest mismatch')
                require_no_existing_process(plan)
                identity_directory = publish_first_identity(req, plan, binary, manifest)
                report.update(state='installed_stopped', installed=True, business_ready=False, binary_sha256=req['binary_sha256'],
                              component=req['component'], version=manifest['version'], release_id=manifest['release_id'],
                              warning=plan['warning'], observed_at=now(), service_enabled=False, identity_directory=identity_directory)
                record['report'] = report
                durable_json(journal, record)
                durable_json(marker, {'schema_version': 1, 'deployment_id': req['operation_id'], 'state': 'succeeded', 'component': req['component'], 'kind': 'host_first_install'})
                return report
            except BaseException:
                # Preserve exact intent and all files. No blind cleanup,
                # restart, overwrite or rollback can erase ambiguous effects.
                record['report'] = dict(report, state='unknown', error='首装未完成；已保留原事务和文件，请补查原任务，不得自动重投')
                durable_json(journal, record)
                raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('phase', choices=('keyscan', 'probe', 'install', 'reconcile'))
    parser.add_argument('--request', type=Path, required=True)
    parser.add_argument('--report', type=Path, required=True)
    parser.add_argument('--package', type=Path)
    args = parser.parse_args()
    req = read_json(args.request)
    report = base_report(req, args.phase)
    try:
        if args.phase == 'install':
            if args.package is None:
                raise ValueError('verified package required')
            report = install(req, args.package)
        else:
            report = globals()[args.phase](req)
    except InstallNotStarted as error:
        report.update(state='failed_before_install', error=str(error)[:4096])
    except (OSError, ValueError, RuntimeError, KeyError, subprocess.SubprocessError) as error:
        report.update(state='unknown' if args.phase in ('install', 'reconcile') else 'failed', error=str(error)[:4096])
    durable_json(args.report, report)
    print(json.dumps({'state': report['state'], 'phase': args.phase}))
    return 0 if report['state'] in ('awaiting_host_key', 'ready_for_install', 'installed_stopped') else 1


if __name__ == '__main__':
    raise SystemExit(main())
