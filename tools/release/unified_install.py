#!/usr/bin/env python3
"""Prepare unified lifecycle integration; never starts/stops any service.

Default is a read-only plan. --apply requires both services conclusively stopped
under an explicitly authorized maintenance window. Profiles are administrator
files, never HTTP inputs. No business config, state, PKI or DB is rewritten.
"""
import argparse
from contextlib import nullcontext
import ipaddress
import json
import os
from pathlib import Path
import pwd
import re
import shlex
import stat
import hashlib
import shutil

from component_deployment import ComponentDeployment
from deployment import durable_json, require_no_unfinished_release, sync_directory, target_lock
from release import Release, atomic_copy
from release_probe import read_json


def fixed_quote(value):
    # Reject systemd specifier/environment expansion, including administrator
    # paths which we cannot preserve literally through a drop-in.
    if not value or any(c in value for c in ('%', '$', '\n', '\r', '\x00')):
        raise ValueError('unsupported systemd argument expansion')
    return '"' + value.replace('\\', '\\\\').replace('"', '\\"') + '"'


def stop_condition(path):
    # ConditionPathExists uses a path parser, not ExecStart's argv parser.
    # Quoting the entire !/path makes older systemd ignore the condition.
    value = str(path)
    if not re.fullmatch(r'/[A-Za-z0-9_./-]+', value):
        raise ValueError('manual-stop condition requires a fixed plain absolute path')
    return '!' + value


def agent_start(original, binary, shared, managers):
    commands = []
    for line in original.replace('\\\n', '').splitlines():
        if line.strip().startswith('ExecStart='):
            text = line.strip().split('=', 1)[1]
            if not text: commands = []
            else: commands.append(text)
    if len(commands) != 1:
        raise ValueError('Agent must have one explicit existing ExecStart')
    args = shlex.split(commands[0])
    if args[:3] != [str(binary), 'agent', 'run']:
        raise ValueError('existing service is not the pinned RCD Agent')
    result = []; skip = False
    for arg in args:
        if skip: skip = False; continue
        option = arg.split('=', 1)[0]
        if option in ('--host-release-dir', '--remote-managers', '--http-listen'):
            skip = '=' not in arg; continue
        result.append(arg)
    if skip: raise ValueError('incomplete existing Agent option')
    result += ['--host-release-dir', str(shared), '--remote-managers', ','.join(managers), '--http-listen', '0.0.0.0:8090']
    return '[Service]\nExecStart=\nExecStart=' + ' '.join(fixed_quote(v) for v in result) + '\n'


def dropin(release):
    control = release.p['control']; unit = control['unit']
    if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.@-]*\.service', unit):
        raise ValueError('invalid fixed service unit')
    if control['kind'] == 'systemd-user':
        user = pwd.getpwnam(control['run_as'])
        root = Path(user.pw_dir) / '.config/systemd/user'
    elif control['kind'] == 'systemd-system':
        root = Path('/etc/systemd/system')
    else: raise ValueError('only existing systemd services can be integrated')
    # Existing deployments use 90-rcd-paths and 95-rcd-maintenance overrides.
    # This must be later, otherwise systemd silently discards our ExecStart.
    return root / (unit + '.d') / 'zz-rcd-unified.conf'


def independent_manager(original, agent_unit):
    """Remove one Agent from the actual declaration; drop-ins cannot reset it."""
    section = ''
    keys = {'Wants', 'Requires', 'Requisite', 'BindsTo', 'PartOf', 'After', 'Before'}
    result = []
    for line in original.replace('\\\n', '').splitlines(keepends=True):
        text = line.strip()
        if text.startswith('['): section = text
        if section == '[Unit]' and '=' in text:
            key, value = text.split('=', 1)
            entries = value.split()
            if key in keys and agent_unit in entries:
                kept = [v for v in entries if v != agent_unit]
                if kept: result.append(key + '=' + ' '.join(kept) + '\n')
                continue
        result.append(line)
    return ''.join(result)


def detach_manager_dependencies(manager, agent_unit, backup):
    unit = manager.p['control']['unit']
    paths = manager.systemctl('show', unit, '-p', 'FragmentPath', '--value').strip().split()
    paths += manager.systemctl('show', unit, '-p', 'DropInPaths', '--value').strip().split()
    for name in paths:
        path = Path(name)
        if not path.is_absolute() or path.is_symlink() or not path.is_file() or path.stat().st_size > 1048576:
            raise ValueError('manager service definition is not an explicit regular file')
        old = path.read_text()
        if agent_unit not in old: continue
        new = independent_manager(old, agent_unit)
        if new == old: continue
        backup.mkdir(mode=0o700, parents=True, exist_ok=True)
        key = hashlib.sha256(name.encode()).hexdigest()[:20]
        saved, staged = backup/(key+'.before'), backup/(key+'.new')
        if saved.exists() or staged.exists():
            raise ValueError('original dependency change exists; inspect installation evidence')
        shutil.copy2(path,saved)
        durable_json(backup/(key+'.json'), {'path':name,'sha256':hashlib.sha256(old.encode()).hexdigest()})
        safe_write(staged,new,0,0)
        staged.chmod(stat.S_IMODE(path.stat().st_mode))
        if path.read_text() != old: raise ValueError('service definition changed during installation')
        atomic_copy(staged,path)


def owner(release):
    if release.p['control']['kind'] == 'systemd-user':
        return pwd.getpwnam(release.p['control']['run_as'])
    name = release.systemctl('show', release.p['control']['unit'], '-p', 'User', '--value').strip()
    return pwd.getpwnam(name or 'root')


def safe_write(path, content, uid, gid):
    # No silent edits of an existing integration or unit; a changed plan needs
    # an explicit maintenance review. Equivalent repeated installation is safe.
    for part in (path, *path.parents):
        if part.is_symlink(): raise ValueError('integration paths cannot contain symlinks')
    raw = content.encode()
    if path.exists():
        if not path.is_file() or path.read_bytes() != raw:
            raise ValueError('existing integration differs; preserve it for review: ' + str(path))
        return
    if not path.parent.exists():
        path.parent.mkdir(parents=True, exist_ok=True)
        os.chown(path.parent, uid, gid)
    with path.open('xb') as stream:
        os.fchmod(stream.fileno(), 0o600);os.fchown(stream.fileno(), uid, gid)
        stream.write(raw);stream.flush();os.fsync(stream.fileno())
    sync_directory(path.parent)


def prepare(profiles, host, managers, fleet, apply=False, *, _bootstrap=None):
    if len(managers) != 3 or len(set(managers)) != 3:
        raise ValueError('the three explicit manager IPs are required')
    for value in managers: ipaddress.ip_address(value)
    manager = Release(read_json(profiles['manager']), host, 'manager')
    agent = Release(read_json(profiles['agent']), host, 'agent')
    lower = Release(read_json(profiles['lower']), host, 'lower')
    if manager.entry.parent != agent.entry.parent or agent.entry.parent != lower.entry.parent:
        raise ValueError('all components must share the same fixed binary directory')
    manager.local_host();agent.local_host()
    if agent.p['lower_profile'] != profiles['lower']:
        raise ValueError('Agent profile must reference this exact lower profile')
    shared = agent.entry.parent
    control = shared / '.rcd-host-control'
    config = manager.runtime / 'fleet-sync.json'
    token = Path(fleet['token_file'])
    if not token.is_absolute() or token.is_symlink() or not token.is_file() or token.stat().st_size > 8192 or token.stat().st_mode & 0o077:
        raise ValueError('native read-only token must already exist in a private file')
    if fleet.get('schema_version') != 1 or len(fleet.get('source_devices', {})) != 3:
        raise ValueError('three explicit stable source/device mappings required')
    aowner, mowner = owner(agent), owner(manager)
    if token.stat().st_uid != mowner.pw_uid:
        raise ValueError('read-only token must belong to the manager service user')
    agent_text = agent_start(agent.systemctl('cat', agent.p['control']['unit']), agent.entry, shared, managers)
    manager_text = ('[Unit]\nConditionPathExists=' + stop_condition(manager.runtime / 'manual-stop.json') +
                    '\n[Service]\nSendSIGKILL=no\nEnvironment=' + fixed_quote('RCD_MANAGER_FLEET_CONFIG=' + str(config)) + '\n')
    result = {'host': host, 'agent_dropin': str(dropin(agent)), 'manager_dropin': str(dropin(manager)),
              'shared_lock': str(shared / '.rcd-host-release.lock'), 'fleet_config': str(config),
              'services_restarted': False, 'applied': False}
    if not apply: return result
    if os.geteuid() != 0: raise ValueError('host integration requires an authorized root installer')
    # The one-time installer may already own the host flock and journal. Only
    # its in-process coordinator can take this path; the CLI cannot supply it.
    with target_lock(manager) if _bootstrap is None else nullcontext():
        if _bootstrap is None:
            require_no_unfinished_release(manager)
        else:
            _bootstrap.assert_owned(manager.entry.parent)
        for release in (manager, agent):
            if not ComponentDeployment(release, 'unified-install').confirmed_service_stopped():
                raise ValueError('manager and Agent must already be safely stopped')
        ComponentDeployment(agent, 'unified-install').ensure_no_process_images({str(agent.entry), str(lower.entry), str(manager.entry)})
        if control.is_symlink(): raise ValueError('host control state cannot be a symlink')
        control.mkdir(mode=0o700, exist_ok=True);os.chown(control, aowner.pw_uid, aowner.pw_gid);os.chmod(control,0o700)
        lock = shared / '.rcd-host-release.lock'
        os.chown(lock, aowner.pw_uid, aowner.pw_gid);os.chmod(lock,0o600)
        for marker in [shared/'.rcd-host-release-active.json', *shared.glob('.*.release-active.json')]:
            if not marker.exists(): continue
            read_json(marker,65536)  # Do not chmod an unvalidated link or arbitrary target.
            if marker.stat().st_nlink != 1: raise ValueError('linked publication marker')
            os.chown(marker, -1, aowner.pw_gid);os.chmod(marker,0o640)
        safe_write(dropin(agent),agent_text,aowner.pw_uid if agent.p['control']['kind']=='systemd-user' else 0,aowner.pw_gid if agent.p['control']['kind']=='systemd-user' else 0)
        safe_write(dropin(manager),manager_text,mowner.pw_uid if manager.p['control']['kind']=='systemd-user' else 0,mowner.pw_gid if manager.p['control']['kind']=='systemd-user' else 0)
        safe_write(config,json.dumps(fleet,ensure_ascii=False,indent=2)+'\n',mowner.pw_uid,mowner.pw_gid)
        detach_manager_dependencies(manager,agent.p['control']['unit'],control/'integration-before')
        # Reload unit definitions only. The caller separately resumes each
        # original maintenance identity under the approved deployment procedure.
        agent.systemctl('daemon-reload');manager.systemctl('daemon-reload')
        result['applied'] = True
    return result


if __name__ == '__main__':
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--profiles',type=Path,required=True,help='administrator JSON map of three profile paths')
    parser.add_argument('--fleet',type=Path,required=True,help='non-secret fleet connection config; token lives separately')
    parser.add_argument('--host',required=True)
    parser.add_argument('--manager-ips',required=True)
    parser.add_argument('--apply',action='store_true')
    args=parser.parse_args()
    print(json.dumps(prepare(read_json(args.profiles),args.host,args.manager_ips.split(','),read_json(args.fleet),args.apply),ensure_ascii=False))
