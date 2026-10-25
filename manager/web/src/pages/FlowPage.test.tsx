import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { MemoryRouter } from 'react-router-dom';
import App from '../App';
import { flowFixture } from '../features/flow/fixture';

const canvasProps=vi.hoisted(()=>({live:false,paused:false}));
vi.mock('../features/flow/FlowCanvas',()=>({laneY:(i:number,n:number)=>48+(i+0.5)*(654/Math.max(n,1)),default:(props:{live:boolean;paused:boolean})=>{Object.assign(canvasProps,props);return <canvas data-testid="flow-canvas"/>;}}));
const matrixProps=vi.hoisted(()=>({live:false,paused:false,sources:[] as unknown[]}));
vi.mock('../features/flow/MatrixCanvas',()=>({default:(props:{live:boolean;paused:boolean;sources:unknown[]})=>{Object.assign(matrixProps,props);return <canvas data-testid="matrix-canvas"/>;}}));
let fetchMock: ReturnType<typeof vi.fn>;
beforeEach(()=>{
  fetchMock=vi.fn(async(input:RequestInfo|URL)=>new Response(JSON.stringify(String(input).includes('/points?')?{items:[{id:'9CYE91GH201_READY',type:'UInt32',value:1,status:0}],total:1,offset:0}:flowFixture()),{status:200,headers:{'Content-Type':'application/json'}}));
  vi.stubGlobal('fetch',fetchMock);
});
afterEach(()=>{vi.unstubAllGlobals();window.localStorage.clear();});
function mount(existing?:QueryClient,entry='/flow'){const client=existing??new QueryClient({defaultOptions:{queries:{retry:false,gcTime:0}}});const view=render(<QueryClientProvider client={client}><MemoryRouter initialEntries={[entry]}><App/></MemoryRouter></QueryClientProvider>);return Object.assign(client,{unmount:view.unmount});}
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
it('switches to the routing matrix, remembers it and lets the address choose',async()=>{
  const first=mount();await screen.findByText('监视连接正常',{exact:false});
  expect(screen.getByRole('button',{name:'管线'})).toHaveAttribute('aria-pressed','true');
  fireEvent.click(screen.getByRole('button',{name:'矩阵'}));
  expect(await screen.findByLabelText('来源吞吐轨迹与会话选源矩阵')).toBeInTheDocument();
  expect(screen.queryByTestId('flow-canvas')).not.toBeInTheDocument();
  expect(screen.getByText('事件时间线')).toBeInTheDocument();expect(screen.getByText('站点阵列')).toBeInTheDocument();
  expect(window.localStorage.getItem('rcd.flow.view')).toBe('matrix');
  first.unmount();first.clear();
  const second=mount();await screen.findByText('监视连接正常',{exact:false});
  expect(screen.getByRole('button',{name:'矩阵'})).toHaveAttribute('aria-pressed','true');
  expect(matrixProps.live).toBe(true);expect(matrixProps.sources).toHaveLength(1);
  second.unmount();second.clear();
  mount(undefined,'/flow?view=pipeline');await screen.findByText('监视连接正常',{exact:false});
  expect(screen.getByTestId('flow-canvas')).toBeInTheDocument();
});
it('opens source detail from a matrix row',async()=>{
  mount(undefined,'/flow?view=matrix');await screen.findByText('监视连接正常',{exact:false});
  fireEvent.click(screen.getByRole('button',{name:/采集主机46/}));
  expect(await screen.findByRole('dialog')).toBeInTheDocument();
  expect(await screen.findByText('9CYE91GH201_READY')).toBeInTheDocument();
});
