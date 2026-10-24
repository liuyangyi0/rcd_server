#!/usr/bin/env python3
"""Register a hash-verified legacy communication image without installing it.

This imports the original source/checksum evidence, not a filename's version.
It neither creates a successful deployment nor makes the image an upgrade or
rollback package. Unknown build time stays unknown; no application is executed.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import subprocess
import tempfile


def digest(path):
    value = hashlib.sha256()
    with Path(path).open('rb') as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b''):
            value.update(chunk)
    return value.hexdigest()


def checked_file(path, expected):
    path = Path(path)
    if not re.fullmatch(r'[a-f0-9]{64}', expected):
        raise ValueError('explicit SHA-256 required')
    if path.is_symlink() or not path.is_file() or digest(path) != expected:
        raise ValueError('original evidence checksum mismatch: ' + str(path))
    return path


def identity(binary, binary_sha, evidence_root, checksums, checksums_sha,
             base_manifest, base_manifest_sha, release_id):
    if not re.fullmatch(r'legacy-communicate-[A-Za-z0-9_.-]{1,80}', release_id):
        raise ValueError('explicit legacy communication registration ID required')
    binary = checked_file(binary, binary_sha)
    with binary.open('rb') as stream:
        header = stream.read(20)
    if header[:7] != b'\x7fELF\x02\x01\x01' or int.from_bytes(header[18:20], 'little') != 62:
        raise ValueError('legacy import supports verified Linux x86_64 images only')
    root = Path(evidence_root).resolve(strict=True)
    checksums = checked_file(checksums, checksums_sha)
    if checksums.stat().st_size > 1 << 20:
        raise ValueError('source checksum record too large')
    files = {}
    for line in checksums.read_text(encoding='utf-8').splitlines():
        match = re.fullmatch(r'([a-f0-9]{64})  (source/[^\r\n]+)', line)
        if not match:
            raise ValueError('invalid original source checksum record')
        name = match[2]
        parts = PurePosixPath(name).parts
        if '\\' in name or '..' in parts or name in files:
            raise ValueError('unsafe or duplicate source checksum path')
        path = root.joinpath(*parts)
        if not path.resolve(strict=True).is_relative_to(root) or any(
                root.joinpath(*parts[:i]).is_symlink() for i in range(1, len(parts) + 1)):
            raise ValueError('source checksum path escapes original snapshot')
        checked_file(path, match[1])
        files[name] = match[1]
    required = {'source/communicate/Cargo.toml', 'source/communicate/Cargo.lock',
                'source/communicate/src/main.rs'}
    if not required.issubset(files):
        raise ValueError('original communication source and dependency lock required')
    cargo = (root / 'source/communicate/Cargo.toml').read_text(encoding='utf-8')
    section = re.search(r'(?ms)^\[package\]\s*\n(.*?)(?=^\[|\Z)', cargo)
    version = re.findall(r'^version\s*=\s*"([0-9]+\.[0-9]+\.[0-9]+)"\s*$',
                         section[1] if section else '', re.M)
    if len(version) != 1:
        raise ValueError('unambiguous original package version required')
    base = json.loads(checked_file(base_manifest, base_manifest_sha).read_text(encoding='utf-8'))
    commit = base.get('declared_base_commit')
    if not isinstance(commit, str) or not re.fullmatch(r'[a-f0-9]{40}', commit):
        raise ValueError('original declared base commit required')
    source_sha = hashlib.sha256(json.dumps(files, sort_keys=True,
                                           separators=(',', ':')).encode()).hexdigest()
    build = {'schema_version': 1, 'component': 'communicate', 'version': version[0],
             'release_id': release_id, 'commit': commit, 'built_at': '',
             'source_sha256': source_sha, 'dirty': True, 'target': 'linux/x86_64'}
    return {'schema_version': 1,
            'components': {'rcd_communicate': {'sha256': binary_sha, 'build_info': build}},
            'provenance': {'kind': 'verified_legacy_source_record',
                           'source_record_sha256': checksums_sha,
                           'source_file_count': len(files),
                           'base_manifest_sha256': base_manifest_sha,
                           'build_time_known': False,
                           'embedded_build_identity': False,
                           'deployment_performed': False}}


def register(record, binary, running_image, releases):
    expected = record['components']['rcd_communicate']['sha256']
    if digest(running_image) != expected:
        raise ValueError('running image does not match the original release')
    root = Path(releases)
    if not root.is_absolute() or root.is_symlink() or not root.is_dir():
        raise ValueError('existing administrator-owned release root required')
    destination = root / record['components']['rcd_communicate']['build_info']['release_id']
    if destination.exists() or destination.is_symlink():
        if (destination.is_symlink() or
                json.loads((destination / 'identity-public.json').read_text()) != record or
                digest(destination / 'rcd_communicate') != expected):
            raise ValueError('immutable registration conflict')
        return {'changed': False, 'directory': str(destination)}
    with tempfile.TemporaryDirectory(prefix='.legacy-identity-', dir=root) as directory:
        stage = Path(directory)
        stage.chmod(0o755)
        artifact = stage / 'rcd_communicate'
        shutil.copyfile(binary, artifact)
        artifact.chmod(0o444)
        with artifact.open('rb') as stream:
            os.fsync(stream.fileno())
        if digest(artifact) != expected or digest(running_image) != expected:
            raise ValueError('image changed while registering')
        with (stage / 'identity-public.json').open('x', encoding='utf-8') as out:
            json.dump(record, out, indent=2, sort_keys=True)
            out.flush()
            os.fsync(out.fileno())
        (stage / 'identity-public.json').chmod(0o644)
        # The inspector never observes a partially copied image/identity pair.
        # Keep TemporaryDirectory's own path in place for ordinary cleanup.
        published = stage / 'published'
        published.mkdir(mode=0o755)
        artifact.rename(published / artifact.name)
        (stage / 'identity-public.json').rename(published / 'identity-public.json')
        published.rename(destination)
        fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    return {'changed': True, 'directory': str(destination)}


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--binary', type=Path, required=True)
    p.add_argument('--binary-sha256', required=True)
    p.add_argument('--evidence-root', type=Path, required=True)
    p.add_argument('--source-checksums', type=Path, required=True)
    p.add_argument('--source-checksums-sha256', required=True)
    p.add_argument('--base-manifest', type=Path, required=True)
    p.add_argument('--base-manifest-sha256', required=True)
    p.add_argument('--release-id', required=True)
    p.add_argument('--running-pid', type=int, required=True)
    p.add_argument('--host', required=True)
    p.add_argument('--apply', action='store_true')
    a = p.parse_args()
    if (os.geteuid() != 0 or a.running_pid < 1 or
            a.host not in subprocess.check_output(['hostname', '-I'], text=True).split()):
        p.error('root on the explicit target host and a current process PID required')
    record = identity(a.binary, a.binary_sha256, a.evidence_root, a.source_checksums,
                      a.source_checksums_sha256, a.base_manifest, a.base_manifest_sha256,
                      a.release_id)
    image = Path('/proc') / str(a.running_pid) / 'exe'
    before = image.stat()
    if digest(image) != a.binary_sha256 or digest('/opt/rcd/bin/rcd_communicate') != a.binary_sha256:
        p.error('running, installed and original communication images must match')
    result = register(record, a.binary, image, Path('/opt/rcd/releases')) if a.apply else {'changed': False}
    after = image.stat()
    if (before.st_dev, before.st_ino) != (after.st_dev, after.st_ino):
        raise RuntimeError('running image changed during registration; re-inspect before claiming a version')
    print(json.dumps(dict(result, identity=record, running_pid=a.running_pid,
                          services_changed=False), sort_keys=True))


if __name__ == '__main__':
    main()
