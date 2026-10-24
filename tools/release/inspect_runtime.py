#!/usr/bin/env python3
"""Read-only software inventory; never executes a discovered application binary.

An entry file version is not proof that a running process uses that version.
Only a verified release manifest matching the process image provides identity.
No business configuration, environment, command payload, or certificate is read.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
from application_profile import application_slug

# Read-only helpers travel together in the private Ansible scratch directory.
FIELDS = ('schema_version', 'component', 'version', 'release_id', 'commit', 'built_at', 'source_sha256', 'dirty', 'target')
BINARY_COMPONENTS = {'RcdManager': 'manager', 'RcdAgent': 'agent', 'rcd_server': 'lower',
                     'rcd_communicate': 'communicate'}


def application_id(value):
    return application_slug(value)


def digest(path):
    value = hashlib.sha256()
    with Path(path).open('rb') as file:
        for chunk in iter(lambda: file.read(1024 * 1024), b''):
            value.update(chunk)
    return value.hexdigest()


def known_images(releases):
    images = {}
    conflicts = set()
    for path in sorted(list(releases.glob('*/manifest.json')) + list(releases.glob('*/identity-public.json'))):
        try:
            if path.is_symlink() or path.stat().st_size > 8 * 1024 * 1024:
                continue
            manifest = json.loads(path.read_text())
            for name, component in manifest.get('components', {}).items():
                if name not in BINARY_COMPONENTS and not application_id(name):
                    continue
                artifact = path.parent / name
                if artifact.is_symlink() or digest(artifact) != component['sha256']:
                    continue
                identity = {key: value for key, value in component['build_info'].items() if key in FIELDS}
                key = (name, component['sha256'])
                if key in images and images[key] != identity:
                    conflicts.add(key)
                images[key] = identity
        except (OSError, ValueError, KeyError, TypeError, AttributeError):
            # Broken/unknown releases do not invent a version for an image.
            continue
    return {key: value for key, value in images.items() if key not in conflicts}


def image_inventory(entry, running_image, name, images):
    result = {'entry': str(entry), 'component': BINARY_COMPONENTS.get(name, name),
              'running': running_image is not None}
    try:
        result['entry_sha256'] = digest(entry)
        result['entry_build'] = images.get((name, result['entry_sha256']))
    except OSError as error:
        result['entry_error'] = str(error)
    if running_image is not None:
        try:
            before = os.stat(running_image)
            result['running_path'] = os.readlink(running_image)
            result['running_sha256'] = digest(running_image)
            after = os.stat(running_image)
            if (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns) != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns):
                raise OSError('process image changed during inspection')
            result['running_build'] = images.get((name, result['running_sha256']))
            result['restart_required_for_entry'] = result.get('entry_sha256') != result['running_sha256']
        except OSError as error:
            result['running_error'] = str(error)
            result['running'] = None  # exited or inaccessible, not "stopped"
            result.pop('running_build', None)
            result.pop('running_sha256', None)
            result.pop('restart_required_for_entry', None)
    return result


def inspect(component, unit, scope, instance, run_as=None, release_roots=(), images=None):
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.@-]*\.service', unit):
        raise ValueError('one explicit systemd service unit is required')
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,127}', instance):
        raise ValueError('invalid instance identifier')
    args = ['systemctl'] + (['--user'] if scope == 'user' else [])
    if run_as and scope == 'user':
        import pwd
        if not re.fullmatch(r'[A-Za-z_][A-Za-z0-9_-]{0,31}', run_as):
            raise ValueError('invalid service account')
        uid = pwd.getpwnam(run_as).pw_uid
        if os.geteuid() != uid:
            if os.geteuid() != 0:
                raise ValueError('cross-user inspection requires root')
            args = ['runuser', '-u', run_as, '--', 'env', 'XDG_RUNTIME_DIR=/run/user/' + str(uid),
                    'DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/' + str(uid) + '/bus'] + args
    output = subprocess.run(args + ['show', unit, '-p', 'LoadState', '-p', 'ActiveState',
                                      '-p', 'SubState', '-p', 'MainPID', '-p', 'NRestarts', '-p', 'UnitFileState'],
                                     text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10)
    service = dict(line.split('=', 1) for line in output.stdout.splitlines() if '=' in line)
    if output.returncode and service.get('LoadState') != 'not-found':
        raise RuntimeError('systemd inspection could not confirm service state')
    if images is None:
        images = verified_images(release_roots)
    main_pid = int(service.get('MainPID', 0))
    result = {'schema_version': 1, 'component': component, 'instance_id': instance, 'unit': unit,
              'scope': scope, 'service': service, 'processes': []}
    name = {'manager': 'RcdManager', 'lower-stack': 'RcdAgent', 'communicate': 'rcd_communicate'}.get(component, component)
    entry = (Path('/opt/rcd/apps') / component / 'bin' / component) if application_id(component) else Path('/opt/rcd/bin') / name
    result['processes'].append(image_inventory(entry,
                               Path('/proc') / str(main_pid) / 'exe' if main_pid else None, name, images))
    if main_pid == 0 and service.get('LoadState') == 'not-found':
        try:
            entry.lstat()
        except FileNotFoundError:
            result['processes'][0].update(installed=False, entry_error='', running=False)
        except OSError:
            pass
    if component == 'lower-stack':
        # Inspect only children of the exact service PID. Do not adopt, signal,
        # or expose their command line/environment.
        children = set()
        children_known = main_pid == 0
        for task in (Path('/proc') / str(main_pid) / 'task').glob('*/children'):
            try:
                children.update(int(pid) for pid in task.read_text().split())
                children_known = True
            except OSError:
                children_known = False
                break
        lower_count = 0
        for pid in sorted(children):
            image = Path('/proc') / str(pid) / 'exe'
            try:
                image_name = os.readlink(image)
                filename = Path(image_name[:-10] if image_name.endswith(' (deleted)') else image_name).name
            except OSError:
                children_known = False
                continue
            if filename == 'rcd_server':
                item = image_inventory(Path('/opt/rcd/bin/rcd_server'), image, 'rcd_server', images)
                item['pid'] = pid
                result['processes'].append(item)
                lower_count += 1
        result['owned_lower_count'] = lower_count
        if not lower_count:
            item = image_inventory(Path('/opt/rcd/bin/rcd_server'), None, 'rcd_server', images)
            # An absent Agent is not proof that no unowned collector is alive.
            # Only a readable, running Agent's child set can confirm "stopped".
            if not children_known or not main_pid:
                item['running'] = None
                item['running_error'] = 'owning Agent is stopped or its children cannot be inspected'
            result['processes'].append(item)
        if lower_count > 1:
            result['inspection_error'] = 'multiple lower processes owned by the Agent'
    current = subprocess.run(args + ['show', unit, '-p', 'MainPID', '-p', 'ActiveState', '-p', 'LoadState'],
                             text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10)
    failed = current.returncode and service.get('LoadState') != 'not-found'
    current = dict(line.split('=', 1) for line in current.stdout.splitlines() if '=' in line)
    if failed or any(current.get(key) != service.get(key) for key in ('MainPID', 'ActiveState', 'LoadState')):
        result['inspection_error'] = 'service changed during inspection'
        for item in result['processes']:
            item['running'] = None
            item['running_error'] = result['inspection_error']
            item.pop('running_build', None)
            item.pop('running_sha256', None)
    return result


def verified_images(release_roots):
    roots = [Path('/opt/rcd/releases')] + [Path(root) for root in release_roots]
    images, conflicts = {}, set()
    for root in roots:
        if not root.is_absolute() or root.is_symlink():
            raise ValueError('release roots must be fixed absolute directories')
        for key, value in known_images(root).items():
            if key in images and images[key] != value:
                conflicts.add(key)
            images[key] = value
    return {key: value for key, value in images.items() if key not in conflicts}


def inspect_host(services, roots, applications=None):
    if not isinstance(services, list) or len(services) not in (2, 3):
        raise ValueError('fixed manager/lower-stack service bindings required')
    if {s.get('component') for s in services} not in ({'manager', 'lower-stack'}, {'manager', 'lower-stack', 'communicate'}):
        raise ValueError('unexpected service bindings')
    services = list(services)
    manager = next(s for s in services if s['component'] == 'manager')
    owner = manager.get('run_as')
    if not owner or manager['scope'] != 'user':
        raise ValueError('verified service account required for host inspection')
    if not any(s['component'] == 'communicate' for s in services):
        services.append({'component': 'communicate', 'unit': 'rcd-communicate.service',
                         'scope': 'user', 'instance': 'communicate', 'run_as': owner})
    applications = [] if applications is None else applications
    if not isinstance(applications, list) or len(applications) > 32 or len({a.get('component') for a in applications}) != len(applications):
        raise ValueError('bounded registered application list required')
    roots = list(roots)
    for app in applications:
        component = app.get('component')
        if not application_id(component):
            raise ValueError('invalid registered application')
        services.append({'component': component, 'unit': 'rcd-app-' + component + '.service',
                         'scope': 'user', 'instance': component, 'run_as': owner})
        roots.append('/var/lib/rcd-release-platform/artifacts/' + component)
    roots.append('/var/lib/rcd-release-platform/artifacts/communicate')
    # Hash immutable release entries once for the whole host, not per component.
    images = verified_images(roots)
    probes = []
    for service in services:
        try:
            probes.append(inspect(service['component'], service['unit'], service['scope'],
                                  service['instance'], service.get('run_as'), roots, images))
        except Exception:
            component = service['component']
            names = ['agent', 'lower'] if component == 'lower-stack' else [component]
            probes.append({'schema_version': 1, 'component': component,
                           'inspection_error': 'service/process observation unavailable',
                           'processes': [{'component': name, 'running': None} for name in names]})
    try:
        from host_metrics import collect_host_status
        status = collect_host_status()
    except Exception:
        import datetime
        status = {'schema_version': 1, 'available': False,
                  'observed_at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
                  'error': 'host metrics unavailable; software observations retained'}
    return {'schema_version': 1, 'probes': probes, 'host_status': status}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--component', choices=['manager', 'lower-stack', 'communicate'])
    parser.add_argument('--unit')
    parser.add_argument('--scope', choices=['system', 'user'])
    parser.add_argument('--instance')
    parser.add_argument('--services')
    parser.add_argument('--applications', default='[]')
    parser.add_argument('--run-as')
    parser.add_argument('--release-root', action='append', default=[])
    parser.add_argument('--release-roots', default='[]')
    args = parser.parse_args()
    roots = json.loads(args.release_roots)
    if not isinstance(roots, list) or any(not isinstance(v, str) for v in roots) or len(roots)>64:
        parser.error('expected at most 64 administrator-owned release roots')
    if args.services:
        print(json.dumps(inspect_host(json.loads(args.services), args.release_root+roots,
                                     json.loads(args.applications))))
    else:
        if not all((args.component,args.unit,args.scope,args.instance)):
            parser.error('single-service bindings or --services are required')
        print(json.dumps(inspect(args.component, args.unit, args.scope, args.instance, args.run_as, args.release_root+roots)))


if __name__ == '__main__':
    main()
