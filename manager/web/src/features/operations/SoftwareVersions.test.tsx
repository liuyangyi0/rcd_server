import { render, screen, fireEvent } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { SoftwareVersionTable, type SoftwareVersion } from './SoftwareVersions';

const build = { version:'1.0', release_id:'release-1', commit:'abc',source_sha256:'123',built_at:'2026-09-28T00:00:00Z',dirty:false,target:'linux/amd64' };
const items: SoftwareVersion[] = [
 {name:'当前管理端',component:'manager',endpoint:'opc.tcp://manager:4840',state:'online',cached:false,build},
 {source_id:'a',name:'来源 A',component:'lower',endpoint:'opc.tcp://a:4841',state:'offline',cached:true,build,observed_at:'2026-09-28T01:00:00Z'},
 {source_id:'b',name:'来源 B',component:'lower',endpoint:'opc.tcp://b:4841',state:'online',cached:false,build:{...build,version:'2.0'}},
 {source_id:'a',name:'来源 A',component:'agent',endpoint:'',state:'unconfigured',cached:false},
];
describe('SoftwareVersionTable',()=>{
 it('retains stale version and distinguishes unknown agent',()=>{
  render(<SoftwareVersionTable items={items}/>);
  expect(screen.getByText('离线 / 过期缓存')).toBeInTheDocument();
  expect(screen.getByText('未绑定')).toBeInTheDocument();
  expect(screen.getByText(/同类组件版本不一致/)).toBeInTheDocument();
 });
 it('filters by component and text without mixing identities',()=>{
  render(<SoftwareVersionTable items={items}/>);
  fireEvent.change(screen.getByLabelText('软件组件筛选'),{target:{value:'lower'}});
  fireEvent.change(screen.getByLabelText('搜索软件版本'),{target:{value:'来源 B'}});
  expect(screen.getByText('共 1 项')).toBeInTheDocument();
  expect(screen.getByText('2.0')).toBeInTheDocument();
  expect(screen.queryByText('未绑定')).not.toBeInTheDocument();
 });
});
