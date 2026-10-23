// Run Chromium on 46 only. Live acceptance is read/validate only: no save,
// restart, source mutation, or TX writes. Credentials arrive on stdin.
const {chromium}=require('/home/lenovo/code/rcd_manager_test/browser-tools/node_modules/playwright');
const fs=require('node:fs');const assert=require('node:assert/strict');
(async()=>{
 let input='';for await(const c of process.stdin)input+=c;
 const credentials=JSON.parse(input);input='';
 const out=process.argv[2];assert.ok(out.startsWith('/home/lenovo/code/rcd_communicate_fixes_')&&!fs.existsSync(out));
 fs.mkdirSync(out,{mode:0o700});
 const browser=await chromium.launch({headless:true});
 const context=await browser.newContext({viewport:{width:1366,height:768},locale:'zh-CN'});
 const page=await context.newPage();const errors=[];const unexpected=[];
 page.on('pageerror',e=>errors.push(e.message));
 await context.route('**/api/**',async route=>{
   const r=route.request();const u=new URL(r.url());
   if(!['GET','HEAD'].includes(r.method())&&!['/api/v1/auth/login','/api/v1/sources/communicate-46/operations/config/validate','/api/v1/sources/communicate-46/operations/config/prepare'].includes(u.pathname)){
     unexpected.push({method:r.method(),path:u.pathname});await route.abort();return;
   }
   await route.continue();
 });
 try{
  const base='http://192.168.10.46:8087';
  const login=await context.request.post(base+'/api/v1/auth/login',{headers:{Origin:base},data:{username:'admin',password:credentials.password}});
  credentials.password='';assert.ok(login.ok(),'manager login failed');
  await page.goto(base+'/#/sources?operations=communicate-46');
  await page.getByRole('tab',{name:'配置',exact:true}).click();
  await page.getByLabel('配置文件',{exact:true}).selectOption('configuration-set.toml');
  const editor=page.getByLabel('配置内容 configuration-set.toml',{exact:true});
  await editor.waitFor({timeout:30000});
  const original=await editor.inputValue();
  assert.ok(original.includes('config_toml')&&original.includes('points_csv'));
  assert.ok(original.includes('enabled = false'));
  await page.getByRole('button',{name:'校验当前草稿',exact:true}).click();
  await page.getByText('当前草稿校验通过',{exact:true}).waitFor({timeout:30000});
  await page.screenshot({path:out+'/configuration-set-1366.png',fullPage:true});
  await page.setViewportSize({width:1920,height:1080});
  await page.screenshot({path:out+'/configuration-set-1920.png',fullPage:true});
  assert.deepEqual(errors,[]);assert.deepEqual(unexpected,[]);
  fs.writeFileSync(out+'/result.json',JSON.stringify({passed:true,errors,unexpected,configuration_saved:false,validation_passed:true,hardware_writes:0},null,2),{mode:0o600});
  console.log('configuration set visible and validated; no configuration or hardware writes');
 }catch(e){await page.screenshot({path:out+'/failure.png',fullPage:true}).catch(()=>{});throw e;}
 finally{await browser.close();}
})().catch(e=>{console.error(e);process.exitCode=1;});
