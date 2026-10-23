"""Read-only contract discovery for the operator UI; never starts a task."""
import json
from connect_release import connection

api=connection()
projects=api('/projects')
for project in projects:
    if project['name']!='RCD 运维':continue
    prefix='/project/'+str(project['id'])
    templates=api(prefix+'/templates')
    print(json.dumps({'project':project,'templates':templates},ensure_ascii=False))
    tasks=api(prefix+'/tasks')
    print(json.dumps({'recent_tasks':tasks[:5]},ensure_ascii=False))
    inspect_ids={t['id'] for t in templates if t['playbook']=='tools/ansible/inspect.yml'}
    task=next((t for t in tasks if t['template_id'] in inspect_ids and t['status']=='success'),None)
    if task:
        output=api(prefix+'/tasks/'+str(task['id'])+'/output')
        print(json.dumps({'inspection_task':task,'output':output},ensure_ascii=False))
