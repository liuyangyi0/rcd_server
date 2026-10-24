#!/usr/bin/env python3
"""Apply the RCD source extension to a pinned, isolated Semaphore checkout.

This patches official source before compilation, not browser/runtime resources.
Does not build, install, access live configuration or start any service.
"""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess

ROOT = Path(__file__).resolve().parent


def apply(source):
    source = source.resolve(strict=True)
    lock = json.loads((ROOT / 'upstream.json').read_text(encoding='utf-8'))
    commit = subprocess.check_output(['git', '-C', str(source), 'rev-parse', 'HEAD'], text=True).strip()
    if commit != lock['commit']:
        raise ValueError('refusing an unpinned Semaphore checkout')
    edits = {
        'db/Migration.go': [('\t\t{Version: "2.19.14"},', '\t\t{Version: "2.19.14"},\n\t\t{Version: "2.19.16.rcd1"},\n\t\t{Version: "2.19.16.rcd2"},\n\t\t{Version: "2.19.16.rcd3"},\n\t\t{Version: "2.19.16.rcd4"},')],
        'api/router.go': [('\tprojectUserAPI := authenticatedAPI.PathPrefix("/project/{project_id}").Subrouter()', '\tprojects.RegisterRCDRoutes(authenticatedAPI, store)\n\n\tprojectUserAPI := authenticatedAPI.PathPrefix("/project/{project_id}").Subrouter()')],
        'api/system_info.go': [('\t"net/http"', '\t"net/http"\n\t"os"'), ('type SystemInfo struct {', 'type SystemInfo struct {\n\tRCDPackages bool `json:"rcd_packages"`'), ('\tbody := SystemInfo{', '\tbody := SystemInfo{\n\t\tRCDPackages: os.Getenv("SEMAPHORE_RCD_PACKAGES_PATH") != "",')],
        'web/src/router/index.js': [('const routes = [', "const routes = [\n  {\n    path: '/project/:projectId/rcd/devices',\n    component: () => import('@/views/rcd/Devices.vue'),\n  },\n  {\n    path: '/project/:projectId/rcd/packages',\n    component: () => import('@/views/rcd/Packages.vue'),\n  },")],
        'web/src/App.vue': [
            ('      dark\n      :color="darkMode ? \'#003236\' : \'#005057\'"', '      :dark="!(systemInfo || {}).rcd_packages"\n      :color="(systemInfo || {}).rcd_packages ? \'#f1f3f6\' : (darkMode ? \'#003236\' : \'#005057\')"'),
            ('      const items = [];\n\n      if (this.project.type', "      const items = [];\n      if ((this.systemInfo || {}).rcd_packages) {\n        items.push({\n          key: 'rcd-devices',\n          icon: 'mdi-monitor',\n          title: '设备与版本',\n          to: `${base}/rcd/devices`,\n          testId: 'sidebar-rcd-devices',\n        });\n        items.push({\n          key: 'rcd-packages',\n          icon: 'mdi-package-variant-closed',\n          title: '软件包',\n          to: `${base}/rcd/packages`,\n          testId: 'sidebar-rcd-packages',\n        });\n      }\n\n      if (this.project.type"),
        ],
        'services/tasks/TaskPool.go': [
            (') (newTask db.Task, err error) {\n\ttaskObj.Created = tz.Now()', ') (newTask db.Task, err error) {\n\treleaseUnlock, releaseErr := p.prepareRCDRelease(&taskObj, projectID, userID)\n\tif releaseUnlock != nil { defer releaseUnlock() }\n\tif releaseErr != nil { return newTask, &RCDTaskAdmissionError{Err: releaseErr} }\n\tunlock, existing, inspectionErr := p.prepareRCDInspection(&taskObj, projectID)\n\tif unlock != nil { defer unlock() }\n\tif inspectionErr != nil { return newTask, &RCDTaskAdmissionError{Err: inspectionErr} }\n\tif existing != nil { return *existing, nil }\n\ttaskObj.Created = tz.Now()'),
            ('\ttaskRunner := NewTaskRunner(newTask, p, username, p.keyInstallationService)', '\tif err = p.attachRCDRelease(newTask); err != nil { return }\n\ttaskRunner := NewTaskRunner(newTask, p, username, p.keyInstallationService)'),
            ('func (p *TaskPool) StopTask(targetTask db.Task, forceStop bool) error {', 'func (p *TaskPool) StopTask(targetTask db.Task, forceStop bool) error {\n\tif err := rcdRejectNativeStop(p.store, targetTask); err != nil { return err }'),
            ('\t\tjob = &LocalExecutor{\n\t\t\tTask:         taskRunner.Task,', '\t\tjob = &LocalExecutor{\n\t\t\tRCDStore: p.store,\n\t\t\tTask:         taskRunner.Task,'),
            ('\t\tjob = &LocalExecutor{\n\t\t\tTask:         tr.Task,', '\t\tjob = &LocalExecutor{\n\t\t\tRCDStore: p.store,\n\t\t\tTask:         tr.Task,'),
        ],
        'services/tasks/local_executor.go': [
            ('type LocalExecutor struct {', 'type LocalExecutor struct {\n\tRCDStore db.Store'),
            ('\tdefer t.Cleanup()\n', '\tdefer t.Cleanup()\n\tdefer func() { recordRCDLaunchFailure(t.RCDStore, t.Task, err) }()\n'),
            ('\textraVars["semaphore_vars"] = vars\n\n\treturn', '\textraVars["semaphore_vars"] = vars\n\terr = rcdInspectionExtraVars(t.RCDStore, t.Task, extraVars)\n\tif err == nil { err = rcdReleaseExtraVars(t.RCDStore, t.Task, extraVars) }\n\n\treturn'),
        ],
        'db_lib/AnsiblePlaybook.go': [
            ('\n\t"github.com/creack/pty"', ''),
            ('ptmx, err := pty.Start(cmd)', 'ptmx, err := startAnsiblePTY(cmd)'),
            ('\t\treturn err\n\t}\n\n\tgo func() {', '\t\treturn &AnsibleLaunchError{Err: err}\n\t}\n\n\tgo func() {'),
        ],
        'services/tasks/TaskRunner.go': [
            ('\tif t.Task.Status.IsFinished() {\n\t\tdesc +=', '\tif t.Task.Status.IsFinished() {\n\t\tCollectRCDInspection(t.pool.store, t.Task)\n\t\tCollectRCDOperation(t.pool.store, t.Task)\n\t\tgo t.pool.scheduleRCDPostRelease(t.Task)\n\t\tdesc +='),
        ],
    }
    edits['web/src/router/index.js'].extend([
        ("    redirect: '/project/:projectId/history',", "    component: () => import('@/views/rcd/Home.vue'),"),
        ("const routes = [", "const routes = [\n  { path: '/project/:projectId/rcd/operations', component: () => import('@/views/rcd/Operations.vue') },"),
    ])
    edits['web/src/App.vue'].extend([
        ('          key="dashboard"', '          v-if="!(systemInfo || {}).rcd_packages"\n          key="dashboard"'),
        ("{{ $t('more') }}", "{{ (systemInfo || {}).rcd_packages ? '高级管理' : $t('more') }}"),
        ("    pinnedNavItemsList() {\n      return", "    pinnedNavItemsList() {\n      if ((this.systemInfo || {}).rcd_packages) return this.navItems.filter((item) => item.key.startsWith('rcd-'));\n      return"),
        ("    unpinnedNavItems() {\n      return", "    unpinnedNavItems() {\n      if ((this.systemInfo || {}).rcd_packages) return this.navItems.filter((item) => !item.key.startsWith('rcd-'));\n      return"),
        ("      // Workflows is a Pro feature; hide the nav item unless it is licensed.", "      if ((this.systemInfo || {}).rcd_packages) {\n        items.push({\n          key: 'rcd-operations', icon: 'mdi-progress-clock', title: '操作任务', to: `${base}/rcd/operations`,\n        });\n        items.push({\n          key: 'native-history', icon: 'mdi-history', title: '全部原生任务', to: `${base}/history`,\n        });\n      }\n      // Workflows is a Pro feature; hide the nav item unless it is licensed."),
    ])
    edits['web/src/views/project/Templates.vue'] = [
        ('    createTask(itemId) {\n      this.itemId = itemId;', "    async createTask(itemId) {\n      const { data: info } = await axios.get('/api/info');\n      if (info.rcd_packages) {\n        const { data: template } = await axios.get(`/api/project/${this.projectId}/templates/${itemId}`);\n        if (template.playbook === 'tools/ansible/control-platform.yml') {\n          await this.$router.push(`/project/${this.projectId}/rcd/devices`);\n          return;\n        }\n        if (['tools/ansible/release-platform.yml', 'tools/ansible/semaphore-release.yml'].includes(template.playbook)) {\n          await this.$router.push(`/project/${this.projectId}/rcd/devices?upgrade=1`);\n          return;\n        }\n      }\n      this.itemId = itemId;"),
    ]
    # Host enrollment uses the existing task admission/executor/finalizer, not
    # an HTTP SSH worker or a second queue. All anchors remain upstream-owned.
    for name, pairs in list(edits.items()):
        updated = []
        for old, new in pairs:
            if name == 'db/Migration.go':
                new = new.replace('{Version: "2.19.16.rcd4"},', '{Version: "2.19.16.rcd4"},\n\t\t{Version: "2.19.16.rcd5"},\n\t\t{Version: "2.19.16.rcd6"},')
            if name == 'services/tasks/TaskPool.go':
                new = new.replace('\n\treleaseUnlock, releaseErr :=', '\n\thostUnlock, hostErr := p.prepareRCDHostSetup(&taskObj, projectID, userID)\n\tif hostUnlock != nil { defer hostUnlock() }\n\tif hostErr != nil { return newTask, &RCDTaskAdmissionError{Err: hostErr} }\n\treleaseUnlock, releaseErr :=')
                new = new.replace('\tif err = p.attachRCDRelease(newTask);', '\tif err = p.attachRCDHostSetup(newTask); err != nil { return }\n\tif err = p.attachRCDRelease(newTask);')
                new = new.replace('\n\tif err := rcdRejectNativeStop', '\n\tif err := rcdRejectHostSetupStop(p.store, targetTask); err != nil { return err }\n\tif err := rcdRejectNativeStop')
            if name == 'services/tasks/local_executor.go':
                new = new.replace('\n\n\treturn', '\n\tif err == nil { err = rcdHostSetupExtraVars(t.RCDStore, t.Task, extraVars) }\n\n\treturn')
            if name == 'services/tasks/TaskRunner.go':
                new = new.replace('\t\tCollectRCDInspection', '\t\tCollectRCDHostSetup(t.pool.store, t.Task)\n\t\tCollectRCDInspection')
            updated.append((old, new))
        edits[name] = updated
    previous_path = source / 'rcd-source-record.json'
    previous = json.loads(previous_path.read_text()) if previous_path.is_file() else {}
    if previous and previous.get('upstream', {}).get('commit') != commit:
        raise ValueError('previous source record belongs to another upstream')
    prepared = []
    for name, replacements in edits.items():
        target = source / name
        original = subprocess.check_output(['git', '-C', str(source), 'show', 'HEAD:' + name]).decode('utf-8').replace('\r\n', '\n')
        expected = original
        for old, new in replacements:
            if expected.count(old) != 1:
                raise ValueError('upstream patch anchor changed: ' + name)
            expected = expected.replace(old, new)
        if name.endswith('.go'):
            expected = subprocess.check_output(['gofmt'], input=expected.encode('utf-8')).decode('utf-8')
        current = target.read_text(encoding='utf-8')
        recorded = previous.get('source_files', {}).get(name)
        if current not in (original, expected) and hashlib.sha256(current.encode()).hexdigest() != recorded:
            raise ValueError('preserve unrelated edits; unexpected content: ' + name)
        prepared.append((target, expected))
    for file in sorted((ROOT / 'files').rglob('*')):
        if file.is_symlink():
            raise ValueError('symlinks are not permitted in source extension')
        if file.is_file():
            relative = file.relative_to(ROOT / 'files')
            target = source / relative
            # These are exclusively RCD-owned additions, never upstream files.
            result = subprocess.run(['git', '-C', str(source), 'cat-file', '-e', 'HEAD:' + relative.as_posix()], capture_output=True)
            if result.returncode == 0:
                raise ValueError('RCD source collides with upstream: ' + str(relative))
            content = file.read_text(encoding='utf-8')
            if file.suffix == '.go':
                content = subprocess.check_output(['gofmt'], input=content.encode('utf-8')).decode('utf-8')
            prepared.append((target, content))
    for target, content in prepared:
        target.parent.mkdir(parents=True, exist_ok=True)
        with target.open('w', encoding='utf-8', newline='') as stream:
            stream.write(content)
    # License remains at its upstream location; the build record describes
    # exactly which extension bytes were supplied, including uncommitted edits.
    changes = {str(target.relative_to(source)): hashlib.sha256(content.encode()).hexdigest()
               for target, content in prepared}
    record = {'upstream': lock, 'source_files': changes, 'not_a_deployment': True}
    (source / 'rcd-source-record.json').write_text(json.dumps(record, indent=2) + '\n', encoding='utf-8')
    print(json.dumps(record, indent=2))


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', type=Path, required=True)
    apply(parser.parse_args().source)
