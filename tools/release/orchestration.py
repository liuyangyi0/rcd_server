#!/usr/bin/env python3
"""Ansible adapter. Reuse release transactions; never implement process control here."""
import argparse
from contextlib import contextmanager
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import tempfile

from deployment import Deployment, durable_json, fingerprint, now, sync_directory
from release import Release, absolute, sha, within

RECOVERY = ('resume-deployment', 'finish-deployment', 'rollback-deployment',
            'resume-rollback', 'finish-rollback')


def identifier(value):
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,95}', value):
        raise ValueError('explicit safe operation ID required')
    return value


def verify_staging(release, directory, manifest):
    origin = directory / 'build-manifest.json'
    binding = manifest['staging']
    if (binding.get('schema_version') != 1 or
            binding.get('build_manifest_sha256') != sha(origin) or
            binding.get('profile_sha256') != fingerprint(release.p)):
        raise RuntimeError('staged origin or target profile changed')
    expected = json.loads(origin.read_text())
    expected['host'] = release.p['host']
    expected['staging'] = binding
    if expected != manifest:
        raise RuntimeError('staged build identity changed')


def validate_build(directory, expected_hash, component):
    if not re.fullmatch('[0-9a-f]{64}', expected_hash):
        raise ValueError('pinned build manifest SHA-256 required')
    if sha(directory / 'manifest.json') != expected_hash:
        raise RuntimeError('build manifest digest mismatch')
    manifest = json.loads((directory / 'manifest.json').read_text())
    info = manifest.get('build_info', {})
    if ('staging' in manifest or manifest.get('component') != component or
            info.get('component') != component or info.get('schema_version') != 1 or
            manifest.get('binary_sha256') != sha(directory / 'binary')):
        raise RuntimeError('build component, identity or binary digest mismatch')
    if (not re.fullmatch('[0-9a-f]{64}', info.get('source_sha256', '')) or
            manifest.get('source', {}).get('sha256') != info['source_sha256'] or
            not info.get('release_id') or not info.get('version')):
        raise RuntimeError('recorded build provenance required')
    machine = platform.machine().lower()
    targets = {'x86_64': ('amd64', 'x86_64', 62), 'aarch64': ('arm64', 'aarch64', 183)}
    if machine not in targets:
        raise RuntimeError('unsupported target architecture')
    go_arch, rust_arch, elf_machine = targets[machine]
    if info.get('target') not in ('linux/' + go_arch, 'linux/' + rust_arch):
        raise RuntimeError('build target architecture mismatch')
    with (directory / 'binary').open('rb') as stream:
        header = stream.read(20)
    if (len(header) != 20 or header[:6] != b'\x7fELF\x02\x01' or
            int.from_bytes(header[18:20], 'little') != elf_machine):
        raise RuntimeError('binary ELF architecture mismatch')
    return manifest


def staging_paths(release, incoming, destination):
    incoming, destination = absolute(str(incoming)), absolute(str(destination))
    if (not within(destination, release.root) or destination.resolve() == release.root.resolve() or
            within(incoming, destination) or within(destination, incoming) or
            within(incoming, release.runtime) or within(release.runtime, incoming) or
            within(incoming, release.source) or within(release.source, incoming) or
            within(release.entry, incoming)):
        raise ValueError('staging needs separate incoming and release artifact directories')
    return incoming, destination


def stage(release, incoming, destination, expected_hash):
    """Only copy binary + build identity; never ship a runtime configuration."""
    incoming, destination = staging_paths(release, incoming, destination)
    manifest = validate_build(incoming, expected_hash, release.p['component'])
    binding = {'schema_version': 1, 'build_manifest_sha256': expected_hash,
               'profile_sha256': fingerprint(release.p)}
    target_manifest = dict(manifest, host=release.p['host'], staging=binding)
    if destination.exists():
        if release.verify(destination) != target_manifest:
            raise RuntimeError('immutable staged artifact already exists with different content')
        return {'changed': False, 'artifact': str(destination), 'manifest_sha256': sha(destination / 'manifest.json')}
    destination.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    temporary = Path(tempfile.mkdtemp(prefix='.staging-', dir=destination.parent))
    try:
        shutil.copyfile(incoming / 'binary', temporary / 'binary')
        shutil.copyfile(incoming / 'manifest.json', temporary / 'build-manifest.json')
        (temporary / 'binary').chmod(0o755)
        # Probe only the documented version CLI in an empty directory. This also
        # rejects missing loaders/libraries before any service is stopped.
        with tempfile.TemporaryDirectory(prefix='version-', dir=temporary) as cwd:
            if release.application:
                # Uploaded third-party binaries are never executed for metadata.
                pass
            else:
                args = ['agent', 'version', '--json'] if release.p['component'] == 'agent' else ['version', '--json']
                result = subprocess.run([str(temporary / 'binary')] + args, cwd=cwd,
                                        capture_output=True, text=True, timeout=15, check=True)
                if json.loads(result.stdout) != manifest['build_info']:
                    raise RuntimeError('staged executable build identity mismatch')
        durable_json(temporary / 'manifest.json', target_manifest)
        for name in ('binary', 'build-manifest.json'):
            with (temporary / name).open('rb') as stream:
                os.fsync(stream.fileno())
        release.verify(temporary)
        # An existing directory is never replaced (including a concurrent stage).
        os.rename(temporary, destination)
        sync_directory(destination.parent)
    finally:
        if temporary.exists():
            shutil.rmtree(temporary)  # only the exact mkdtemp directory owned here
    return {'changed': True, 'artifact': str(destination), 'manifest_sha256': sha(destination / 'manifest.json')}


@contextmanager
def orchestration_lock(release):
    import fcntl
    path = release.entry.parent / ('.' + release.entry.name + '.ansible.lock')
    fd = os.open(path, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield
    finally:
        os.close(fd)


def execute_once(release, operation_id, request, function):
    """Persist intent BEFORE delegation; uncertainty never causes an automatic replay."""
    identifier(operation_id)
    with orchestration_lock(release):
        directory = release.root / 'ansible-operations' / operation_id
        path = directory / 'operation.json'
        active = release.entry.parent / ('.' + release.entry.name + '.ansible-active.json')
        if path.exists():
            saved = json.loads(path.read_text())
            if saved['request'] != request:
                raise RuntimeError('operation ID content conflict')
            if saved['state'] != 'succeeded':
                raise RuntimeError('operation result unknown; inspect and select explicit recovery, do not replay')
            return {'changed': False, 'already_executed': True, 'record': str(path), 'result': saved['result']}
        if active.exists():
            previous = json.loads(active.read_text())
            if previous['state'] != 'succeeded':
                old = previous['request']
                recovering = (request['action'] in RECOVERY and old['mode'] == 'maintenance' and
                              request['deployment_id'] == old['deployment_id'])
                rolling_back = (request['action'] == 'rollback' and old['mode'] == 'manual' and
                                request['recover_operation_id'] == previous['operation_id'])
                if not (recovering or rolling_back):
                    raise RuntimeError('unfinished Ansible operation; new deployment ID cannot bypass it')
                if old['profile_sha256'] != request['profile_sha256']:
                    raise RuntimeError('recovery target profile changed')
        directory.mkdir(parents=True, mode=0o700)
        saved = {'schema_version': 1, 'operation_id': operation_id, 'request': request,
                 'state': 'intent', 'started_at': now()}
        durable_json(path, saved)
        durable_json(active, saved)
        try:
            result = function()
        except BaseException as error:
            saved.update(state='unknown', error_type=type(error).__name__, ended_at=now())
            durable_json(path, saved)
            durable_json(active, saved)
            raise
        saved.update(state='succeeded', result=result, ended_at=now())
        durable_json(path, saved)
        durable_json(active, saved)
        return {'changed': True, 'already_executed': False, 'record': str(path), 'result': result}


def perform(release, action, artifact, mode, deployment_id, initiator, restore_data):
    # All controls, CAS, backups, runtime checks and recovery semantics remain
    # owned by the existing release implementation, not by Ansible handlers.
    if release.p['component'] == 'communicate' or release.application:
        if action != 'deploy' or mode != 'maintenance':
            raise RuntimeError('communicate supports only controlled stopped-service publishing; use original operation evidence for recovery')
        from component_deployment import ComponentDeployment
        return ComponentDeployment(release, deployment_id).deploy(artifact, initiator)
    if action == 'deploy':
        if mode == 'maintenance':
            return Deployment(release, deployment_id).deploy(artifact, initiator)
        return release.deploy(artifact)
    if action == 'rollback':
        return release.rollback(artifact, restore_data=restore_data)
    if action == 'resume-deployment':
        from resume import ResumeStoppedDeployment
        return ResumeStoppedDeployment(release, deployment_id).resume(initiator)
    if action == 'finish-deployment':
        from recovery import FinishDeployment
        return FinishDeployment(release, deployment_id).finish(initiator)
    from rollback_deployment import RollbackBeforeStart
    operation = RollbackBeforeStart(release, deployment_id)
    return {'rollback-deployment': operation.rollback, 'resume-rollback': operation.resume_rollback,
            'finish-rollback': operation.finish}[action](initiator)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('preflight', 'history', 'validate-staging', 'stage', 'verify-running', 'inspect-deployment', 'deploy', 'rollback') + RECOVERY)
    parser.add_argument('--profile', required=True)
    parser.add_argument('--host', required=True)
    parser.add_argument('--component', required=True)
    parser.add_argument('--artifact', type=Path)
    parser.add_argument('--incoming', type=Path)
    parser.add_argument('--manifest-sha256', default='')
    parser.add_argument('--operation-id', default='')
    parser.add_argument('--deployment-id', default='')
    parser.add_argument('--mode', choices=('manual', 'maintenance'), default='maintenance')
    parser.add_argument('--initiator', default='')
    parser.add_argument('--recover-operation-id', default='')
    parser.add_argument('--confirm-live-target', default='')
    parser.add_argument('--confirm-no-active-commands', action='store_true')
    parser.add_argument('--confirm-restore-backup-data', action='store_true')
    args = parser.parse_args()
    release = Release(json.loads(Path(args.profile).read_text()), args.host, args.component)
    release.local_host()
    if args.action in ('validate-staging', 'stage', 'verify-running', 'deploy', 'rollback') and args.artifact is None:
        parser.error('--artifact required')
    if args.action == 'preflight':
        result = {'changed': False, 'preflight': release.preflight()}
    elif args.action == 'history':
        # Do not expose backup contents, credentials or raw transaction tokens.
        records = []
        for path in sorted(release.root.glob('**/backup.json')):
            if path.is_symlink() or not within(path, release.root):
                continue
            item = json.loads(path.read_text())
            records.append({'kind': 'backup', 'path': str(path.parent),
                            'binary_sha256': item.get('binary_sha256')})
        for pattern, kind in [('ansible-operations/*/operation.json', 'operation'),
                              ('deployments/*/deployment.json', 'deployment')]:
            for path in sorted(release.root.glob(pattern)):
                if path.is_symlink() or not within(path, release.root):
                    continue
                item = json.loads(path.read_text())
                records.append({'kind': kind, 'id': path.parent.name, 'state': item.get('state'),
                                'action': item.get('request', {}).get('action'),
                                'started_at': item.get('started_at')})
        result = {'changed': False, 'records': records, 'empty': not records}
    elif args.action in ('validate-staging', 'stage'):
        if args.incoming is None:
            parser.error('--incoming required')
        if args.action == 'validate-staging':
            staging_paths(release, args.incoming, args.artifact)
            result = {'changed': False, 'staging_paths_valid': True}
        else:
            result = stage(release, args.incoming, args.artifact, args.manifest_sha256)
    elif args.action == 'verify-running':
        manifest = release.verify(args.artifact)
        result = {'changed': False, 'software_acceptance': release.accept(manifest['binary_sha256']),
                  'build_info': manifest.get('build_info'), 'business_acceptance': 'not_performed'}
    elif args.action == 'inspect-deployment':
        result = {'changed': False, 'result': Deployment(release, args.deployment_id).inspect()}
    else:
        if args.confirm_live_target != args.host + '/' + args.component or not args.confirm_no_active_commands or not args.initiator:
            parser.error('explicit target, maintenance window and initiator required')
        if args.confirm_restore_backup_data and args.action != 'rollback':
            parser.error('data restore only supported for explicit manual rollback')
        if args.action == 'rollback' and args.mode != 'manual':
            parser.error('backup rollback requires explicit manual mode; fenced jobs use recover.yml')
        if args.action in RECOVERY or (args.action == 'deploy' and args.mode == 'maintenance'):
            identifier(args.deployment_id)
            if args.component != 'lower' or args.mode != 'maintenance':
                parser.error('maintenance transactions are lower-only; no automatic manual fallback')
        request = {'action': args.action, 'host': args.host, 'component': args.component,
                   'artifact': str(args.artifact) if args.artifact else None, 'mode': args.mode,
                   'deployment_id': args.deployment_id, 'initiator': args.initiator,
                   'profile_sha256': fingerprint(release.p), 'restore_data': args.confirm_restore_backup_data,
                   'recover_operation_id': args.recover_operation_id,
                   'tool_sha256': fingerprint({name: sha(Path(__file__).parent / name) for name in
                       ('orchestration.py', 'release.py', 'deployment.py', 'recovery.py', 'resume.py', 'rollback_deployment.py')})}
        if args.artifact:
            name = 'backup.json' if args.action == 'rollback' else 'manifest.json'
            # Missing artifacts still fail safely in the underlying preflight.
            path = args.artifact / name
            request['artifact_manifest_sha256'] = sha(path) if path.is_file() else None
        result = execute_once(release, args.operation_id, request, lambda: perform(
            release, args.action, args.artifact, args.mode, args.deployment_id,
            args.initiator, args.confirm_restore_backup_data))
    print(json.dumps(result, ensure_ascii=False))


if __name__ == '__main__':
    main()
