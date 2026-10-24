"""Fixed definition boundary for registered single-binary Linux applications.

No package can select its install path, service, user, argument list or scripts.
The profile/ownership marker is provisioned by the native first-install job.
"""
import json
from pathlib import Path
import re

BUILTINS = ('lower', 'manager', 'agent', 'communicate')
RESERVED = ('system', 'systemd', 'semaphore', 'rcd', 'root', 'sudo', 'ssh', 'sshd', 'init', 'login', 'kernel', 'lower-stack')


def application_slug(value):
    return (isinstance(value, str) and re.fullmatch(r'[a-z][a-z0-9-]{0,31}', value) is not None
            and value not in BUILTINS + RESERVED and not value.endswith('-')
            and '--' not in value and not value.startswith('rcd-'))


def component_slug(value):
    return value in BUILTINS or application_slug(value)


def valid_arguments(args):
    return (isinstance(args, list) and len(args) <= 16
            and all(isinstance(a, str) and re.fullmatch(r'[A-Za-z0-9_./:=,@+ -]{1,256}', a)
                    and a.strip() == a and '..' not in a for a in args)
            and sum(len(a) for a in args) <= 2048)


def unit_text(entry, runtime, args):
    def quote(value):
        if any(c in value for c in ('\n', '\r', '\0', '%', '$')):
            raise ValueError('systemd expansion is not supported')
        return '"' + value.replace('\\', '\\\\').replace('"', '\\"') + '"'
    return ('[Unit]\nDescription=RCD registered application\nConditionPathExists=!' + runtime + '/manual-stop.json\n\n[Service]\nType=simple\n'
            'WorkingDirectory=' + quote(runtime) + '\nExecStart=' + ' '.join(map(quote, [entry, *args])) +
            '\nRestart=no\nSendSIGKILL=no\nTimeoutStopSec=30s\nKillSignal=SIGTERM\nKillMode=control-group\n'
            '\n[Install]\nWantedBy=default.target\n')


def validate_application_profile(profile, check_owner=True):
    component = profile.get('component')
    app = profile.get('application')
    if (not application_slug(component) or not isinstance(app, dict)
            or set(app) != {'schema_version', 'project_id', 'component', 'definition_sha256', 'arguments', 'graceful_shutdown_confirmed'}
            or app.get('schema_version') != 1 or type(app.get('project_id')) is not int or app['project_id'] < 1
            or app.get('component') != component or not re.fullmatch('[a-f0-9]{64}', app.get('definition_sha256', ''))
            or not valid_arguments(app.get('arguments')) or app.get('graceful_shutdown_confirmed') is not True):
        raise ValueError('registered immutable application identity is required')
    entry = '/opt/rcd/apps/' + component + '/bin/' + component
    runtime = '/var/lib/rcd/apps/' + component
    control = profile.get('control', {})
    if (profile.get('executable') != entry or profile.get('runtime_dir') != runtime
            or profile.get('work_dir', runtime) != runtime or profile.get('host_lock_dir') != '/opt/rcd/bin'
            or profile.get('databases', []) or profile.get('config_files', []) or profile.get('config_dirs', [])
            or profile.get('health_url') or profile.get('opc_port') or profile.get('lower_profile')
            or control.get('kind') != 'systemd-user' or control.get('unit') != 'rcd-app-' + component + '.service'
            or not re.fullmatch(r'[A-Za-z_][A-Za-z0-9_-]{0,31}', control.get('run_as', ''))
            or control.get('run_as') == 'root' or set(control) != {'kind', 'unit', 'run_as'}):
        raise ValueError('application paths, systemd user unit and shared host lock must be fixed by registration')
    if check_owner:
        marker = Path(runtime) / '.rcd-application-owner.json'
        if marker.is_symlink() or not marker.is_file() or marker.stat().st_size > 8192:
            raise ValueError('application ownership marker missing or unsafe')
        if json.loads(marker.read_text()) != app:
            raise ValueError('application belongs to another project or definition; refusing replacement')
    return app
