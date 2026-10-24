"""Apply the small UI-only overlay to pristine Semaphore v2.19.12 source."""
from pathlib import Path
import argparse
import shutil

parser=argparse.ArgumentParser()
parser.add_argument('source',type=Path)
args=parser.parse_args()
parser.error('Retired overlay: use official Semaphore native Views. This source is retained for history only.')
root=args.source.resolve()
overlay=Path(__file__).resolve().parent
def replace(path, old, new, count=1):
    text=path.read_text()
    if text.count(old)!=count:raise RuntimeError('upstream context changed: '+str(path))
    path.write_text(text.replace(old,new))

route=root/'web/src/router/index.js'
replace(route,"const routes = [", "const routes = [\n  {\n    path: '/project/:projectId/operations',\n    component: () => import('../views/project/rcd/RcdOperations.vue'),\n  },")
# Keep original pages available via advanced=1; the old bookmarked task list
# and default dashboard now land on the operator view for the RCD project only.
replace(route,'export default router;', '''router.beforeEach((to, from, next) => {
  if (to.params.projectId === '1' && !to.query.advanced
      && /^\/project\/1\/(history|templates)$/.test(to.path)) {
    next('/project/1/operations');
  } else { next(); }
});
export default router;''')
app=root/'web/src/App.vue'
replace(app,'v-if="$route.path.startsWith(\'/project/\')"', 'v-if="$route.path.startsWith(\'/project/\') && !$route.path.endsWith(\'/operations\')"')
replace(app,'<v-main>\n      <router-view','<v-main :style="$route.path.endsWith(\'/operations\') ? {padding: \'0\'} : {}">\n      <router-view')
replace(app,'<v-list class="pt-0" v-if="project">','''<v-list class="pt-0" v-if="project">
        <v-list-item v-if="projectId === 1" :to="`/project/${projectId}/operations`">
          <v-list-item-icon><v-icon>mdi-server</v-icon></v-list-item-icon>
          <v-list-item-content><v-list-item-title>设备与软件</v-list-item-title></v-list-item-content>
        </v-list-item>''')
destination=root/'web/src/views/project/rcd'
destination.mkdir(exist_ok=False)
for name in ('RcdOperations.vue','operator.css','model.mjs'):
    shutil.copy2(overlay/name,destination/name)
print('UI overlay applied; no backend, schema, authentication or task execution changes')
