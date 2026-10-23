#!/usr/bin/env python3
"""Fixed-profile Semaphore adapter; structured evidence, never stdout inference."""
import argparse
import json
import os
from pathlib import Path
import tempfile
import shutil

from component_deployment import ComponentDeployment, compatible
from deployment import durable_json
from orchestration import identifier, stage
from release import Release, sha
from release_probe import public_error


def stage_public(r, incoming, manifest_sha, operation):
    if incoming.is_symlink() or not incoming.is_dir() or sha(incoming / 'manifest.json') != manifest_sha:
        raise RuntimeError('transferred package manifest mismatch')
    m = json.loads((incoming / 'manifest.json').read_text())
    if m.get('schema_version') != 1 or m.get('component') != r.p['component'] or m.get('binary_sha256') != sha(incoming / 'binary'):
        raise RuntimeError('transferred package component or hash mismatch')
    fields = ('component', 'version', 'release_id', 'commit', 'source_sha256', 'dirty', 'built_at', 'target')
    info = dict({k: m[k] for k in fields}, schema_version=1)
    build = {'component': m['component'], 'build_info': info, 'source': {'sha256': m['source_sha256']},
             'binary_sha256': m['binary_sha256'], 'compatibility': m['compatibility']}
    compatible(r, build)
    destination = r.root / ('platform-' + identifier(operation))
    with tempfile.TemporaryDirectory(prefix='build-', dir=incoming) as scratch:
        temp = Path(scratch)
        shutil.copyfile(incoming / 'binary', temp / 'binary')
        durable_json(temp / 'manifest.json', build)
        result = stage(r, temp, destination, sha(temp / 'manifest.json'))
    # Identity follows the existing inspector's hash-backed release manifest
    # convention. No service state is inferred from this on-disk information.
    name = {'lower': 'rcd_server', 'manager': 'RcdManager', 'agent': 'RcdAgent',
            'communicate': 'rcd_communicate'}.get(r.p['component'], r.p['component'])
    artifact_name = destination / name
    if not artifact_name.exists():
        os.link(destination / 'binary', artifact_name)
    durable_json(destination / 'identity-public.json', {'components': {name: {'sha256': m['binary_sha256'], 'build_info': info}}})
    return result


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('action', choices=('stage', 'execute', 'inspect'))
    p.add_argument('--profile', type=Path, required=True)
    p.add_argument('--host', required=True)
    p.add_argument('--component', required=True)
    p.add_argument('--operation-id', required=True)
    p.add_argument('--task-id', type=int, required=True)
    p.add_argument('--device-id', required=True)
    p.add_argument('--host-alias', required=True)
    p.add_argument('--incoming', type=Path)
    p.add_argument('--manifest-sha256')
    p.add_argument('--initiator', default='')
    p.add_argument('--commands-paused', action='store_true')
    p.add_argument('--project-id', type=int)
    p.add_argument('--definition-sha256')
    args = p.parse_args()
    for value in (args.operation_id, args.device_id, args.host_alias): identifier(value)
    if args.task_id < 1: p.error('native task id required')
    r = Release(json.loads(args.profile.read_text()), args.host, args.component)
    if r.application and (r.p['application']['project_id'] != args.project_id or r.p['application']['definition_sha256'] != args.definition_sha256):
        raise ValueError('registered project/application identity does not match installed target')
    if not r.application and (args.project_id is not None or args.definition_sha256 is not None):
        raise ValueError('builtin component cannot receive application overrides')
    r.local_host()
    artifact = r.root / ('platform-' + args.operation_id)
    report = {'schema_version': 1, 'operation_id': args.operation_id, 'task_id': args.task_id,
              'device_id': args.device_id, 'host_alias': args.host_alias, 'state': 'failed_before_install',
              'error': '', 'stages': [], 'acceptance': {}, 'binary_sha256': ''}
    output = r.root / 'platform-reports' / (args.operation_id + '.json')
    if args.action == 'stage':
        if args.incoming is None or not args.incoming.is_absolute(): p.error('absolute staging directory required')
        result = stage_public(r, args.incoming, args.manifest_sha256, args.operation_id)
        print(json.dumps(dict(result, report_path=str(output))))
        return
    d = ComponentDeployment(r, args.operation_id)
    d.report_binding = {k: report[k] for k in ('operation_id', 'task_id', 'device_id', 'host_alias')}
    if args.action == 'inspect':
        # This is deliberately only evidence retrieval, never automatic resume.
        if output.is_file(): print(output.read_text()); return
        if d.path.is_file():
            record = json.loads(d.path.read_text())
            report.update(state='unknown', stages=public_stages(record), error='执行记录存在但缺少最终验收；未重新派发')
        print(json.dumps(report, ensure_ascii=False)); return
    if not args.commands_paused or not args.initiator or len(args.initiator) > 128: p.error('current command pause and actor required')
    output.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    if output.exists() or d.path.exists():
        raise RuntimeError('operation was already attempted; inspect the original evidence, never execute again')
    failed = False
    try:
        result = d.deploy(artifact, args.initiator)
        report.update(state=result['state'], acceptance=result['acceptance'], binary_sha256=result['acceptance']['binary_sha256'])
    except Exception as error:
        failed = True
        report['state'] = (d.record or {}).get('state', 'failed_before_install')
        stages = (d.record or {}).get('stages', [])
        detail = public_error(error, stages[-1]['stage'] if stages else '')
        report['error'] = detail['message']
        report['error_code'] = detail['code']
    finally:
        report['stages'] = public_stages(d.record or {})
        durable_json(output, report)
    print(json.dumps(dict(report, report_path=str(output)), ensure_ascii=False))
    if failed or report['state'] != 'succeeded': raise SystemExit(1)


def public_stages(record):
    fields = ('stage', 'state', 'started_at', 'ended_at', 'error_type', 'error')
    return [{k: v for k, v in stage.items() if k in fields} for stage in record.get('stages', [])]


if __name__ == '__main__':
    main()
