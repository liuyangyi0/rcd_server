// Remote46 UI fixture. Built resources, no live APIs or writes.
const fs = require('node:fs'), path = require('node:path'), http = require('node:http'), assert = require('node:assert/strict');
const { chromium } = require(process.env.RCD_PLAYWRIGHT_MODULE || 'playwright');
const root = path.resolve(__dirname, '../..'), dist = path.join(root,'manager/web/dist');
const output = process.env.RCD_BROWSER_OUTPUT;
if (!output || !path.isAbsolute(output)) throw new Error('explicit output directory required');
const build = {version:'0.6.1-test',release_id:'isolated-version-test',commit:'a'.repeat(40),source_sha256:'b'.repeat(64),built_at:'2026-09-28T00:00:00Z',target:'linux/amd64',dirty:true};
let rows = [{name:'当前管理端',component:'manager',endpoint:'opc.tcp://manager:4840',build,state:'online',cached:false,observed_at:'2026-09-28T09:00:00Z'},
 {source_id:'a',name:'采集机 A',component:'lower',endpoint:'opc.tcp://192.0.2.30:4841',build,state:'offline',cached:true,observed_at:'2026-09-28T08:00:00Z'},
 {source_id:'a',name:'采集机 A',component:'agent',endpoint:'http://192.0.2.30:8090',build:{...build,version:'0.6.0'},state:'online',cached:false},
 {source_id:'b',name:'采集机 B',component:'lower',endpoint:'opc.tcp://192.0.2.58:4841',state:'unsupported',cached:false},
 {source_id:'b',name:'采集机 B',component:'agent',endpoint:'',state:'unconfigured',cached:false}];
let fail=false;
const server=http.createServer((req,res)=>{
 const candidate=path.resolve(dist,'.'+new URL(req.url,'http://localhost').pathname);
 if(!candidate.startsWith(dist+path.sep)){res.writeHead(403).end();return;}
 const file=fs.existsSync(candidate)&&fs.statSync(candidate).isFile()?candidate:path.join(dist,'index.html');
 res.setHeader('Content-Type',({'.js':'text/javascript','.css':'text/css','.html':'text/html','.svg':'image/svg+xml'})[path.extname(file)]||'application/octet-stream');fs.createReadStream(file).pipe(res);
});
(async()=>{
 fs.mkdirSync(output,{recursive:true});await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
 const browser=await chromium.launch({headless:true});const errors=[],writes=[];
 try{
  const page=await browser.newPage({viewport:{width:1440,height:1050}});page.on('pageerror',e=>errors.push(e.message));
  await page.route('**/api/v1/**',async route=>{
   if(route.request().method()!=='GET'){writes.push(route.request().url());return route.abort();}
   const p=new URL(route.request().url()).pathname;let value={items:[],total:0};
   if(p.endsWith('/auth/session'))value={authenticated:true,username:'test'};
   else if(p==='/api/v1/software-versions'){
    if(fail)return route.fulfill({status:503,contentType:'application/json',body:JSON.stringify({message:'隔离测试读取失败'})});
    value={items:rows};
   } else if(p==='/api/v1/system')value={};
   await route.fulfill({contentType:'application/json',body:JSON.stringify(value)});
  });
  await page.route('**/events',route=>route.fulfill({contentType:'text/event-stream',body:': fixture\n\n'}));
  await page.goto(`http://127.0.0.1:${server.address().port}/index.html#/settings`);
  const section=page.getByRole('region',{name:'软件版本总览'});
  await section.getByText('共 5 项').waitFor();await section.scrollIntoViewIfNeeded();
  await section.getByText('离线 / 过期缓存').waitFor();await section.getByText('版本功能不支持').waitFor();
  await section.getByText('查看构建').first().click();await section.getByText('a'.repeat(40),{exact:true}).first().waitFor();
  await page.screenshot({path:path.join(output,'versions-desktop.png')});
  await section.getByLabel('软件组件筛选').selectOption('agent');await section.getByText('共 2 项').waitFor();
  await section.getByLabel('搜索软件版本').fill('不存在');await section.getByText('没有符合条件的组件').waitFor();
  await section.getByLabel('搜索软件版本').fill('');await section.getByLabel('软件组件筛选').selectOption('');
  fail=true;await section.getByRole('button',{name:'刷新版本',exact:true}).click();await section.getByText('隔离测试读取失败',{exact:false}).first().waitFor();
  await section.getByText('离线 / 过期缓存').waitFor();
  await page.setViewportSize({width:390,height:844});await section.scrollIntoViewIfNeeded();
  assert.equal(await section.evaluate(el=>el.scrollWidth<=el.clientWidth+1),true,'section must not overflow mobile page');
  await page.screenshot({path:path.join(output,'versions-mobile.png')});
  assert.deepEqual(errors,[]);assert.deepEqual(writes,[]);
  fs.writeFileSync(path.join(output,'result.json'),JSON.stringify({scenarios:6,page_errors:errors,writes},null,2));
  console.log('PASS: versions, cached/unsupported, details, filtering, error retention, mobile; no writes');
 }finally{await browser.close();server.close();}
})().catch(e=>{console.error(e);server.close();process.exitCode=1;});
