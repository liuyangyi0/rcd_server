import type { FlowSnapshot } from './model';

// Test-only deterministic data. The actual monitor never imports this module.
export function flowFixture(): FlowSnapshot {
  return {generated_at:'2026-10-24T10:00:00Z',instance:'boot1',endpoint:'opc.tcp://192.168.10.46:4840',running:true,pending_commands:0,
    groups:[{id:'serial-acquisition',name:'串口采集组',default_source:'rcd-46',members:['rcd-46']}],
    sources:[{id:'rcd-46',name:'采集主机46',address:'opc.tcp://192.168.10.46:4841',enabled:true,connected:true,subscribed:true,catalog_state:'Ready',samples:'100',last_data_at:'2026-10-24T10:00:00Z',truncated:false,ports:[{id:'/dev/ttyWCH0',name:'ttyWCH0',devices:[{id:'11',name:'站11',group_id:'serial-acquisition',state:1,point_count:30}]}]}],
    sessions:[{id:'public-alias',name:'组态客户端',attached:true,routes:{'serial-acquisition':'rcd-46'},subscriptions:1,monitored_items:30,notifications:{'7':100}}],calculation:{active:true,rules:50,good:40,bad:10,duration_ms:0.8}};
}
