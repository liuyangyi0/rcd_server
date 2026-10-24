#!/usr/bin/env python3
"""Mender inventory adapter using the existing read-only RCD process inspector.

The cache stores only public version evidence. A cached version is never
reported as the current running version when inspection fails or the process
is stopped. No service actions or discovered executable are run.
"""
import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
from urllib.parse import quote

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'release'))
from inspect_runtime import inspect

ROLES = {'manager': 'RcdManager', 'agent': 'RcdAgent', 'lower': 'rcd_server'}
PUBLIC_BUILD = ('version', 'release_id', 'commit', 'source_sha256', 'target')


def validate_profile(profile):
    if set(profile) != {'schema_version', 'node_id', 'name', 'address', 'services'}:
        raise ValueError('inventory profile fields missing or unknown')
    if profile['schema_version'] != 1:
        raise ValueError('unsupported inventory profile')
    for field in ('node_id', 'name', 'address'):
        if not isinstance(profile[field], str) or not profile[field] or len(profile[field]) > 256:
            raise ValueError('invalid inventory identity field')
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,127}', profile['node_id']):
        raise ValueError('invalid stable inventory node ID')
    import ipaddress
    ipaddress.ip_address(profile['address'])
    services = profile['services']
    if not isinstance(services, dict) or set(services) != {'manager', 'lower-stack'}:
        raise ValueError('exact manager and lower-stack service bindings required')
    for binding in services.values():
        if not isinstance(binding, dict) or set(binding) != {'unit', 'scope', 'run_as', 'instance'}:
            raise ValueError('invalid service binding fields')
        if binding['scope'] not in ('system', 'user'):
            raise ValueError('invalid systemd scope')
        if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.@-]*\.service', binding['unit']):
            raise ValueError('invalid service unit')
        if not re.fullmatch(r'[A-Za-z_][A-Za-z0-9_-]{0,31}', binding['run_as']):
            raise ValueError('explicit service owner required')
        if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,127}', binding['instance']):
            raise ValueError('invalid instance')
    return profile


def read_service(role, binding):
    if binding['scope'] == 'system':
        return inspect(role, binding['unit'], 'system', binding['instance'])
    import pwd
    uid = pwd.getpwnam(binding['run_as']).pw_uid
    inspector = Path(__file__).resolve().parents[1] / 'release' / 'inspect_runtime.py'
    args = [sys.executable, str(inspector), '--component', role, '--unit', binding['unit'],
            '--scope', 'user', '--instance', binding['instance']]
    environment = dict(os.environ, XDG_RUNTIME_DIR='/run/user/' + str(uid),
                       DBUS_SESSION_BUS_ADDRESS='unix:path=/run/user/' + str(uid) + '/bus')
    if os.geteuid() != uid:
        if os.geteuid() != 0:
            raise PermissionError('cannot inspect another user systemd scope')
        args = ['runuser', '-u', binding['run_as'], '--', 'env',
                'XDG_RUNTIME_DIR=' + environment['XDG_RUNTIME_DIR'],
                'DBUS_SESSION_BUS_ADDRESS=' + environment['DBUS_SESSION_BUS_ADDRESS']] + args
    output = subprocess.check_output(args, text=True, env=environment, timeout=30,
                                     stderr=subprocess.DEVNULL)
    return json.loads(output)


def summarize(component, snapshot, previous, checked_at):
    result = {'state': 'unknown', 'checked_at': checked_at, 'running_version': '',
              'running_sha256': '', 'restart_required': False,
              'last_confirmed_at': previous.get('last_confirmed_at', ''),
              'last_confirmed_build': previous.get('last_confirmed_build', {})}
    if not isinstance(snapshot, dict) or snapshot.get('service', {}).get('LoadState') != 'loaded':
        return result
    matches = [item for item in snapshot.get('processes', [])
               if Path(item.get('entry', '')).name == ROLES[component]]
    if len(matches) != 1:
        # A missing child on a stopped service is stopped; absent evidence for
        # a running parent is unknown, not a fabricated zero/healthy state.
        if not matches and snapshot.get('service', {}).get('ActiveState') == 'inactive':
            result['state'] = 'stopped'
        return result
    process = matches[0]
    running = process.get('running')
    if running is False:
        result['state'] = 'stopped'
        return result
    if running is not True or not process.get('running_sha256'):
        return result
    result.update(state='running_unverified', running_sha256=process['running_sha256'],
                  restart_required=process.get('restart_required_for_entry', False))
    build = process.get('running_build')
    if not isinstance(build, dict) or not build.get('version'):
        return result
    public = {key: build[key] for key in PUBLIC_BUILD if isinstance(build.get(key), str)}
    result.update(state='running', running_version=public['version'],
                  last_confirmed_at=checked_at, last_confirmed_build=public)
    return result


def attributes(profile, snapshots, previous, checked_at):
    # Changing a service binding cannot carry another process's old identity.
    import hashlib
    fingerprint = hashlib.sha256(json.dumps(profile, sort_keys=True).encode()).hexdigest()
    if previous.get('profile_sha256') != fingerprint:
        previous = {}
    components = {}
    fields = {'rcd_node_id': profile['node_id'], 'rcd_name': profile['name'],
              'rcd_address': profile['address'], 'rcd_checked_at': checked_at,
              'rcd_inventory_schema': '1'}
    for component in ROLES:
        service = 'manager' if component == 'manager' else 'lower-stack'
        result = summarize(component, snapshots.get(service),
                           previous.get('components', {}).get(component, {}), checked_at)
        components[component] = result
        prefix = 'rcd_' + component + '_'
        for field in ('state', 'running_version', 'running_sha256', 'last_confirmed_at'):
            fields[prefix + field] = result[field]
        fields[prefix + 'restart_required'] = str(result['restart_required']).lower()
        for field in PUBLIC_BUILD:
            fields[prefix + 'last_' + field] = result['last_confirmed_build'].get(field, '')
    return fields, {'schema_version': 1, 'profile_sha256': fingerprint, 'components': components}


def write_cache(path, value):
    if not path.is_absolute() or path.is_symlink() or not path.parent.is_dir():
        raise ValueError('existing absolute inventory cache parent required')
    descriptor, name = tempfile.mkstemp(prefix='.inventory-', dir=path.parent)
    try:
        with os.fdopen(descriptor, 'w', encoding='utf-8') as stream:
            json.dump(value, stream, ensure_ascii=False)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(name, path)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--profile', type=Path, required=True)
    parser.add_argument('--cache', type=Path, required=True)
    args = parser.parse_args()
    profile = validate_profile(json.loads(args.profile.read_text()))
    previous = {}
    try:
        if not args.cache.is_symlink() and args.cache.stat().st_size <= 64 * 1024:
            previous = json.loads(args.cache.read_text())
            if not isinstance(previous, dict):
                previous = {}
    except (OSError, ValueError):
        pass
    snapshots = {}
    for role, binding in profile['services'].items():
        try:
            snapshots[role] = read_service(role, binding)
        except (OSError, ValueError, subprocess.SubprocessError):
            snapshots[role] = None
    fields, cache = attributes(profile, snapshots, previous, datetime.now(timezone.utc).isoformat())
    try:
        write_cache(args.cache, cache)
        fields['rcd_inventory_cache_state'] = 'ok'
    except OSError:
        fields['rcd_inventory_cache_state'] = 'write_failed'
    for key, value in sorted(fields.items()):
        print(key + '=' + quote(str(value), safe=' /:._-+'))


if __name__ == '__main__':
    main()
