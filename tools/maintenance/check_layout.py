#!/usr/bin/env python3
"""Read-only repository layout and local Markdown-link checks (run remotely)."""
import pathlib
import re
import sys
import urllib.parse

root = pathlib.Path(__file__).resolve().parents[2]
errors = []
required = [
    'src/main.rs', 'src/lib.rs', 'Cargo.toml', 'Cargo.lock',
    'manager/go.mod', 'manager/go.sum', 'manager/cmd/rcd-manager/main.go',
    'manager/web/embed.go', 'manager/web/dist/embed-placeholder.txt',
    'manager/web/package.json', 'manager/web/package-lock.json',
    'serialport-rs/Cargo.toml', 'vendor/async-opcua-server/Cargo.toml',
    'manager/third_party/awcullen-opcua/go.mod', 'manager/third_party/gopcua-opcua/go.mod',
    'manager/docs/examples/calc-station-state-commented.toml',
    'tools/requirements.txt',
    'tools/opc/opc_client_test.py', 'tools/opc/opc_e2e_serial_test.py',
    'tools/opc/serial_opc_observe.py',
    'tools/serial/serial_parity_analyze.py', 'tools/serial/serial_parity_matrix.py',
    'tools/serial/serial_parity_probe.c', 'tools/serial/serial_parity_report.py',
    'tools/serial/serial_parity_run.py', 'tools/serial/test_serial_parity_matrix.py',
    'tools/fuzz-device/fuzz_device.py', 'tools/fuzz-device/Dockerfile.build',
    'tools/fuzz-device/build_kylin.bat', 'tools/fuzz-device/build_windows.bat',
]
for name in required:
    if not (root / name).is_file():
        errors.append('Missing build/reference input: ' + name)
for name in ['src_backup', 'jisuan.txt', 'REFACTORING.md', 'USER_MANUAL.md']:
    if (root / name).exists():
        errors.append('Legacy root path unexpectedly present: ' + name)

documents = [root/'AGENTS.md', root/'README.md', root/'manager/README.md', root/'manager/web/README.md', root/'artifacts/README.md']
for directory in ['docs', 'manager/docs', 'manager/tools', 'tools']:
    documents.extend((root/directory).rglob('*.md'))
links = 0
for document in documents:
    if not document.is_file():
        errors.append('Missing documentation: ' + str(document.relative_to(root)))
        continue
    text = re.sub(r'```.*?```', '', document.read_text(encoding='utf-8-sig'), flags=re.S)
    for match in re.finditer(r'\]\(([^)\r\n]+)\)', text):
        target = match.group(1)
        if re.match(r'^(?:[a-zA-Z][a-zA-Z0-9+.-]*:|#|/)', target):
            continue
        path = urllib.parse.unquote(re.split('[#?]', target, 1)[0].strip('<>'))
        resolved = (document.parent / path).resolve()
        links += 1
        try:
            resolved.relative_to(root)
            inside = True
        except ValueError:
            inside = False
        if not inside or not resolved.exists():
            errors.append(f'{document.relative_to(root)}: missing/outside link {target}')

for dependency in re.findall(r'path\s*=\s*"([^"]+)"', (root/'Cargo.toml').read_text()):
    if not (root/dependency/'Cargo.toml').is_file():
        errors.append('Missing Rust path dependency: ' + dependency)
for dependency in re.findall(r'=>\s*(\./\S+)', (root/'manager/go.mod').read_text()):
    if not (root/'manager'/dependency/'go.mod').is_file():
        errors.append('Missing Go replacement dependency: ' + dependency)

if errors:
    print('\n'.join(errors), file=sys.stderr)
    sys.exit(1)
print(f'PASS: {len(required)} required inputs, {len(documents)} documents, {links} local links, Rust/Go path dependencies.')
