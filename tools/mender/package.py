#!/usr/bin/env python3
"""Export a verified RCD build as one Mender application Artifact.

Only the binary and a public manifest are exported. Never execute the input
binary, copy the private build manifest, or include runtime configuration.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile

COMPONENTS = ('lower', 'manager', 'agent')
BUILD_FIELDS = ('schema_version', 'component', 'version', 'commit', 'built_at',
                'release_id', 'source_sha256', 'dirty', 'target')
STATE_FORMATS = {'lower': {'command_journal', 'maintenance'},
                 'manager': {'manager_db'}, 'agent': {'agent_state'}}
IDENTIFIER = re.compile(r'[A-Za-z0-9][A-Za-z0-9._+\-]{0,127}\Z')
DIGEST = re.compile(r'[0-9a-f]{64}\Z')


def digest(path):
    value = hashlib.sha256()
    with path.open('rb') as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b''):
            value.update(chunk)
    return value.hexdigest()


def identifier(value, field):
    if not isinstance(value, str) or not IDENTIFIER.fullmatch(value):
        raise ValueError('invalid ' + field)
    return value


def load_object(path, limit=8 * 1024 * 1024):
    if path.is_symlink() or not path.is_file() or path.stat().st_size > limit:
        raise ValueError('regular bounded JSON file required')
    def pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                raise ValueError('duplicate JSON field: ' + key)
            result[key] = value
        return result
    value = json.loads(path.read_text(encoding='utf-8'), object_pairs_hook=pairs)
    if not isinstance(value, dict):
        raise ValueError('JSON object required')
    return value


def validate_compatibility(value, component):
    if set(value) != {'adapter_min_version', 'state_formats', 'component_dependencies'}:
        raise ValueError('compatibility fields missing or unknown')
    version = value['adapter_min_version']
    if not isinstance(version, str) or not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+', version):
        raise ValueError('adapter_min_version must be a numeric semantic version')
    formats = value['state_formats']
    if not isinstance(formats, dict) or set(formats) != STATE_FORMATS[component]:
        raise ValueError('explicit component state compatibility required')
    for bounds in formats.values():
        if (not isinstance(bounds, dict) or set(bounds) != {'min', 'max'} or
                any(type(bounds[key]) is not int for key in ('min', 'max')) or
                not 0 <= bounds['min'] <= bounds['max']):
            raise ValueError('invalid state format compatibility range')
    dependencies = value['component_dependencies']
    if not isinstance(dependencies, dict) or set(dependencies) - (set(COMPONENTS) - {component}):
        raise ValueError('invalid component dependencies')
    for minimum in dependencies.values():
        identifier(minimum, 'dependency version')
    return value


def public_manifest(build, expected_manifest_sha256, component, device_types, compatibility, notes):
    if component not in COMPONENTS:
        raise ValueError('unknown component')
    if not isinstance(expected_manifest_sha256, str) or not DIGEST.fullmatch(expected_manifest_sha256):
        raise ValueError('pinned build manifest SHA-256 required')
    if build.is_symlink() or not build.is_dir():
        raise ValueError('regular build directory required')
    source = build / 'manifest.json'
    if digest(source) != expected_manifest_sha256:
        raise ValueError('build manifest SHA-256 mismatch')
    original = load_object(source)
    info = original.get('build_info', {})
    if not isinstance(info, dict) or any(field not in info for field in BUILD_FIELDS):
        raise ValueError('complete build identity required')
    if (original.get('component') != component or info['component'] != component or
            info['schema_version'] != 1 or type(info['dirty']) is not bool):
        raise ValueError('component or build schema mismatch')
    for key in ('version', 'release_id'):
        identifier(info[key], key)
    if not isinstance(info['commit'], str) or not re.fullmatch('[0-9a-f]{40}', info['commit']):
        raise ValueError('full build commit required')
    if (not isinstance(info['source_sha256'], str) or not DIGEST.fullmatch(info['source_sha256']) or
            original.get('source', {}).get('sha256') != info['source_sha256']):
        raise ValueError('source identity mismatch')
    from datetime import datetime
    if not isinstance(info['built_at'], str):
        raise ValueError('build timestamp required')
    timestamp = datetime.fromisoformat(info['built_at'].replace('Z', '+00:00'))
    if timestamp.tzinfo is None:
        raise ValueError('build timestamp timezone required')
    target_machine = {'linux/amd64': 62, 'linux/x86_64': 62,
                      'linux/arm64': 183, 'linux/aarch64': 183}.get(info['target'])
    binary = build / 'binary'
    if not target_machine or binary.is_symlink() or not binary.is_file():
        raise ValueError('regular supported Linux binary required')
    with binary.open('rb') as stream:
        header = stream.read(20)
    if (len(header) != 20 or header[:6] != b'\x7fELF\x02\x01' or
            int.from_bytes(header[18:20], 'little') != target_machine):
        raise ValueError('binary ELF architecture mismatch')
    binary_digest = digest(binary)
    if original.get('binary_sha256') != binary_digest:
        raise ValueError('binary SHA-256 mismatch')
    if not device_types or len(device_types) != len(set(device_types)):
        raise ValueError('distinct explicit device types required')
    for value in device_types:
        identifier(value, 'device type')
    validate_compatibility(compatibility, component)
    if not isinstance(notes, str) or len(notes.encode('utf-8')) > 16 * 1024 or '\x00' in notes:
        raise ValueError('release notes exceed limit or contain NUL')
    arch = 'amd64' if target_machine == 62 else 'arm64'
    name = f"rcd-{component}-{info['version']}-{arch}-{binary_digest[:12]}"
    return {'schema_version': 1, 'payload_type': 'rcd-component', 'component': component,
            'artifact_name': name, 'device_types': list(device_types),
            'build_info': {field: info[field] for field in BUILD_FIELDS},
            'binary_sha256': binary_digest, 'binary_size': binary.stat().st_size,
            'compatibility': compatibility, 'release_notes': notes}


def export(build, manifest, output, artifact_tool, artifact_tool_sha256):
    if output.suffix != '.mender' or not output.is_absolute() or not output.parent.is_dir():
        raise ValueError('absolute .mender output in an existing directory required')
    if output.exists() or output.is_symlink():
        raise ValueError('output exists; immutable artifacts cannot be overwritten')
    if (not artifact_tool.is_absolute() or artifact_tool.is_symlink() or
            not DIGEST.fullmatch(artifact_tool_sha256) or digest(artifact_tool) != artifact_tool_sha256):
        raise ValueError('pinned absolute mender-artifact executable required')
    with tempfile.TemporaryDirectory(prefix='.rcd-package-', dir=output.parent) as directory:
        stage = Path(directory)
        payload = stage / 'binary'
        shutil.copyfile(build / 'binary', payload)
        if digest(payload) != manifest['binary_sha256']:
            raise ValueError('input binary changed during export')
        metadata = stage / 'rcd-manifest.json'
        metadata.write_text(json.dumps(manifest, ensure_ascii=False, sort_keys=True), encoding='utf-8')
        temporary = stage / 'package.mender'
        args = [str(artifact_tool), 'write', 'module-image', '-T', 'rcd-component',
                '-n', manifest['artifact_name'], '-o', str(temporary),
                '--software-filesystem', 'rcd', '--software-name', manifest['component'],
                '--software-version', manifest['build_info']['version'],
                '--clears-provides', 'rcd.' + manifest['component'] + '.*',
                '-m', str(metadata), '-f', str(payload), '-f', str(metadata)]
        for device_type in manifest['device_types']:
            args.extend(['-c', device_type])
        subprocess.run(args, check=True, timeout=300)
        if not temporary.is_file() or temporary.stat().st_size == 0:
            raise RuntimeError('mender-artifact produced no artifact')
        with temporary.open('rb') as stream:
            os.fsync(stream.fileno())
        # Atomic no-clobber publication, including racing exporters.
        os.link(temporary, output)
        descriptor = os.open(output.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    return {'artifact': str(output), 'artifact_name': manifest['artifact_name'],
            'sha256': digest(output), 'component': manifest['component']}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--build', type=Path, required=True)
    parser.add_argument('--manifest-sha256', required=True)
    parser.add_argument('--component', choices=COMPONENTS, required=True)
    parser.add_argument('--device-type', action='append', required=True)
    parser.add_argument('--compatibility', type=Path, required=True)
    parser.add_argument('--notes', default='')
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--mender-artifact', type=Path, required=True)
    parser.add_argument('--tool-sha256', required=True)
    args = parser.parse_args()
    manifest = public_manifest(args.build, args.manifest_sha256, args.component,
                               args.device_type, load_object(args.compatibility), args.notes)
    print(json.dumps(export(args.build, manifest, args.output, args.mender_artifact,
                            args.tool_sha256), ensure_ascii=False))


if __name__ == '__main__':
    main()
