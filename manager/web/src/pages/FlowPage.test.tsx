import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { MemoryRouter } from 'react-router-dom';
import App from '../App';
import { flowFixture } from '../features/flow/fixture';

const canvasProps=vi.hoisted(()=>({live:false,paused:false}));
vi.mock('../features/flow/FlowCanvas',()=>({laneY:(i:number,n:number)=>48+(i+0.5)*(654/Math.max(n,1)),default:(props:{live:boolean;paused:boolean})=>{Object.assign(canvasProps,props);return <canvas data-testid="flow-canvas"/>;}}));
let fetchMock: ReturnType<typeof vi.fn>;
beforeEach(()=>{
  fetchMock=vi.fn(async(input:RequestInfo|URL)=>new Response(JSON.stringify(String(input).includes('/points?')?{items:[{id:'9CYE91GH201_READY',type:'UInt32',value:1,status:0}],total:1,offset:0}:flowFixture()),{status:200,headers:{'Content-Type':'application/json'}}));
  vi.stubGlobal('fetch',fetchMock);
});
afterEach(()=>vi.unstubAllGlobals());
function mount(existing?:QueryClient){const client=existing??new QueryClient({defaultOptions:{queries:{retry:false,gcTime:0}}});render(<QueryClientProvider client={client}><MemoryRouter initialEntries={['/flow']}><App/></MemoryRouter></QueryClientProvider>);return client;}
it('does not present an old query-cache snapshot as a fresh connection',async()=>{
  const client=new QueryClient({defaultOptions:{queries:{retry:false,gcTime:60000}}});
  const data=flowFixture();data.sources[0].connected=false;data.sources[0].ports[0].devices[0].state=5;
  client.setQueryData(['public-flow'],data,{updatedAt:Date.now()-60000});
  fetchMock.mockImplementation(()=>new Promise(()=>{}));
  mount(client);
  await screen.findByText('监视已断开 / 状态过期',{exact:false});
  expect(canvasProps.live).toBe(false);
  expect(screen.getByRole('alert')).toHaveTextContent('最后收到的快照');
  expect(screen.getByText('0 站')).toBeInTheDocument();
  client.clear();
});
it('opens without login, only calls public GET APIs and shows real route labels',async()=>{
  mount();expect(await screen.findByRole('heading',{name:'RCD 数据流监视'})).toBeInTheDocument();
  expect(await screen.findByText('监视连接正常',{exact:false})).toBeInTheDocument();
  expect(screen.getByText('组态客户端')).toBeInTheDocument();expect(screen.queryByLabelText('密码')).not.toBeInTheDocument();
  for(const [path,options] of fetchMock.mock.calls){expect(String(path)).toContain('/api/v1/public/flow');expect((options as RequestInit|undefined)?.method??'GET').toBe('GET');expect((options as RequestInit).credentials).toBe('omit');}
});
it('supports source detail, actual point values and keyboard closing',async()=>{
  mount();await screen.findByText('监视连接正常',{exact:false});
  fireEvent.click(screen.getByRole('button',{name:/采集主机46/}));
  expect(await screen.findByRole('dialog')).toBeInTheDocument();
  expect(await screen.findByText('9CYE91GH201_READY')).toBeInTheDocument();expect(screen.getByLabelText('筛选串口')).toBeInTheDocument();
  fireEvent.keyDown(window,{key:'Escape'});expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
});
it('stops data activity on failure, retains snapshot and pauses visuals independently',async()=>{
  const client=mount();await screen.findByText('监视连接正常',{exact:false});
  fireEvent.click(screen.getByRole('button',{name:/暂停动画/}));expect(canvasProps.paused).toBe(true);
  fetchMock.mockRejectedValue(new Error('offline'));
  await act(async()=>{await client.invalidateQueries({queryKey:['public-flow']});});
  await waitFor(()=>expect(canvasProps.live).toBe(false));
  expect(screen.getByRole('alert')).toHaveTextContent('最后收到的快照');expect(screen.getByRole('button',{name:/采集主机46/})).toBeInTheDocument();
});
