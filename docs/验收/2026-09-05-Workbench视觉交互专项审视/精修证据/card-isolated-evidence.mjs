/** Synthetic projection states: production card presentation, no backend transition claims. */
import { createRequire } from 'node:module'
import { pathToFileURL, fileURLToPath } from 'node:url'
import { dirname, join, resolve } from 'node:path'
import { mkdtemp, writeFile, rm } from 'node:fs/promises'
import assert from 'node:assert/strict'
const evidence = dirname(fileURLToPath(import.meta.url))
const repo = resolve(evidence, '../../../../')
const web = join(repo, 'workbench/apps/web')
const requireWeb = createRequire(join(web, 'package.json'))
const { createServer } = await import(pathToFileURL(requireWeb.resolve('vite')).href)
const { default: react } = await import(pathToFileURL(requireWeb.resolve('@vitejs/plugin-react')).href)
const { chromium } = requireWeb('playwright')
const fixture = await mkdtemp(join(web, '.card-qa-'))
const result = {provenance:'隔离卡片测试数据，非真实运行；不验证后端过渡',cases:[],transitions:[],visualIssues:[],errors:[],cleanup:false}
let server, browser
try {
 await writeFile(join(fixture,'index.html'), '<!doctype html><html lang="zh-CN"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>隔离卡片测试数据，非真实运行</title></head><body><div id="root"></div><script type="module" src="/main.tsx"></script></body></html>')
 await writeFile(join(fixture,'main.tsx'), `import React, {useState} from 'react'
import {createRoot} from 'react-dom/client'
import {WorkTaskConversationCard} from '../../../packages/client/ui-weave/src/client/WorkTaskPanel.tsx'
import {workTaskModel} from '../../../packages/client/ui-weave/src/client/work-task-model.ts'
import {zh} from '../../../packages/client/ui-weave/src/client/locales.ts'
import '../../../packages/client/ui-theme/src/styles/base.css'
import '../../../packages/client/ui-theme/src/styles/design-platform.css'
import '../../../packages/client/ui-theme/src/styles/scrollbar.css'
import './fixture.css'
const state = {followed:{},tabs:{},selectedMembers:{},reading:{},expandedOutputs:{},outputSelection:{}}
const noop = () => {}
const actions = {toggleFollow:noop,selectTab:noop,selectMember:noop,rememberReading:noop,expandOutput:noop,showOutput:noop}
const t = (key, params={}) => (zh[key] ?? key).replace(/\\{([^}]+)\\}/g, (_,name) => String(params[name] ?? ''))
const output = {id:'fixture-final',title:'隔离评审报告',kind:'final',contentType:'text/markdown',content:'隔离测试内容',preview:'',truncated:false,createdAt:''}
const definitions = [
 ['preparing','准备中',{status:'preparing',runId:'',preparation:{callId:'fixture-build',buildId:'fixture-build',state:'building',error:'',steps:[{id:'review',label:'整理团队方案',status:'running',attempt:1}]}}],
 ['running','执行中',{status:'running',latestStage:'review'}],
 ['waiting','等待补充',{status:'waiting',waitKind:'human'}],
 ['failed','失败',{status:'failed'}],
 ['completed-final','完成 · 有最终成果',{status:'completed',deliverables:[output],completedStages:2}],
 ['completed-missing','完成 · 无最终成果',{status:'completed',completedStages:2}],
 ['stopped','已停止',{status:'stopped'}],
]
function Card({overrides,live=false}) {
 const projection = {...workTaskModel([]),detected:true,runId:'fixture-run',clientRequestId:'fixture-request',teamId:'fixture-team',teamName:'隔离评审团队',observedAt:Date.now(),updatedAt:Date.now(),totalStages:2,completedStages:1,...overrides}
 const props = {useStore:select=>select(state),actions,useChat:select=>select({nodes:{values:()=>[]}}),useProjection:()=>projection,useSessions:select=>select({byId:{fixture:{title:'隔离卡片验收'}}}),sessionId:'fixture',openDetails:noop,stopRun:async()=>null,requestDelivery:async()=>{},t}
 return <WorkTaskConversationCard {...props}/>
}
function App(){
 const [live,setLive] = useState('running')
 return <><header>隔离卡片测试数据，非真实运行 · 不验证后端过渡</header><main><h1>会话卡片 · 状态样本</h1><div className="grid">{definitions.map(([id,title,overrides])=><article data-case={id} key={id}><h2>{title}</h2><Card overrides={overrides}/></article>)}</div><section data-live><h2>同一个卡片 · 显示状态切换</h2><nav>{['running','waiting','completed'].map(status=><button key={status} onClick={()=>setLive(status)}>{status}</button>)}</nav><Card live overrides={{status:live,waitKind:live==='waiting'?'human':'',deliverables:live==='completed'?[output]:[]}}/></section></main></>
}
createRoot(document.getElementById('root')!).render(<App/> )
`)
 await writeFile(join(fixture,'fixture.css'),'body{margin:0;font-family:var(--dsw-font-family);background:var(--dsw-alias-bg-base);color:var(--dsw-alias-label-primary)}body>div>header{padding:12px 16px;background:var(--dsw-alias-bg-module-platform);font-size:13px;line-height:20px;border-bottom:1px solid var(--dsw-alias-border-l2)}main{max-width:1200px;margin:24px auto;padding:0 16px}h1{font-size:22px;line-height:1.4}h2{font-size:13px;font-weight:500;color:var(--dsw-alias-label-secondary);margin:12px 0}.grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:16px 24px}.grid article{min-width:0;padding-bottom:10px;border-bottom:1px solid var(--dsw-alias-border-l2)}nav{display:flex;gap:8px;margin-bottom:12px}nav button{font:inherit;font-size:12px;padding:6px 10px;border:1px solid var(--dsw-alias-border-l2);border-radius:6px;background:var(--dsw-alias-bg-base);color:inherit}[data-live]{margin-top:30px}@media(max-width:620px){.grid{grid-template-columns:1fr}}')
 server = await createServer({configFile:false,root:fixture,plugins:[react()],cacheDir:join(fixture,'cache'),server:{host:'127.0.0.1',port:0,fs:{allow:[join(repo,'workbench')]}},resolve:{dedupe:['react','react-dom'],alias:[{find:'@deepseek-ai/dsh-client-ui-primitives',replacement:join(repo,'workbench/packages/client/ui-primitives/src/index.ts')}]}})
 await server.listen()
 const url='http://127.0.0.1:'+server.httpServer.address().port
 browser=await chromium.launch({headless:true})
 const page=await browser.newPage()
 page.on('pageerror',error=>result.errors.push(error.message))
 const inspect = locator => locator.evaluate(el=>{
  const card=el.querySelector('[data-weave-task-card]')
  const marker=el.querySelector('[data-executing]')
  const stateDot=el.querySelector('[role=status] > span[aria-hidden]')
  return {text:card?.innerText,hasActivity:!!marker,animation:marker?getComputedStyle(marker).animationName:'none',runningAnimations:card?.getAnimations({subtree:true}).filter(a=>a.playState==='running').length,markerStatus:stateDot?.getAttribute('data-status'),markerBackground:stateDot?getComputedStyle(stateDot).backgroundColor:null,activityAnimations:stateDot?.getAnimations().filter(a=>a.playState==='running').length??0,cardWidth:card?.getBoundingClientRect().width,scrollWidth:card?.scrollWidth,clientWidth:card?.clientWidth}
 })
 for(const width of [1440,390]){
  await page.setViewportSize({width,height:1000})
  await page.emulateMedia({reducedMotion:'no-preference'})
  await page.goto(url)
  await page.locator('[data-case=running] [data-executing]').waitFor()
  for(const id of ['preparing','running','waiting','failed','completed-final','completed-missing','stopped']){
   const measured=await inspect(page.locator('[data-case="'+id+'"]'))
   assert.equal(measured.hasActivity,id==='running',id+' activity marker')
   if(id==='running') assert.notEqual(measured.animation,'none')
   else assert.equal(measured.activityAnimations,0)
   assert.ok(measured.scrollWidth<=measured.clientWidth+1,id+' card overflow')
   result.cases.push({width,id,...measured})
   if(id !== 'preparing' && measured.markerBackground === 'rgba(0, 0, 0, 0)') result.visualIssues.push({width,id,issue:'Status marker is transparent; warning token is undefined in current theme source.'})
  }
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth),width)
  await page.screenshot({path:join(evidence,'G-card-states-'+width+'.png'),fullPage:true})
  const live=page.locator('[data-live]')
  const liveCard=await live.locator('[data-weave-task-card]').elementHandle()
  for(const status of ['running','waiting','completed']){
   await live.getByRole('button',{name:status,exact:true}).click()
   const measured=await inspect(live)
   assert.equal(measured.hasActivity,status==='running')
   if(status!=='running') assert.equal(measured.activityAnimations,0)
   assert.equal(await liveCard.evaluate((el)=>el===document.querySelector('[data-live] [data-weave-task-card]')),true)
   result.transitions.push({width,status,...measured})
  }
  await page.emulateMedia({reducedMotion:'reduce'})
  await live.getByRole('button',{name:'running',exact:true}).click()
  await page.waitForTimeout(300)
  const reduced=await inspect(live)
  assert.equal(reduced.hasActivity,true)
  assert.equal(reduced.animation,'none')
  assert.equal(reduced.runningAnimations,0)
  result.transitions.push({width,reducedMotion:true,...reduced})
  for(const card of await page.locator('[data-case]').all()){ const stopped = await inspect(card); assert.equal(stopped.runningAnimations,0,'reduced-motion static card animation') }
  await page.screenshot({path:join(evidence,'G-card-reduced-motion-'+width+'.png'),fullPage:true})
 }
 assert.deepEqual(result.errors,[])
 result.result=result.visualIssues.length ? 'behavior-passed-with-visual-issue' : 'passed'
}catch(error){result.result='failed';result.failure=error.stack;process.exitCode=1}
finally{await browser?.close();await server?.close();await new Promise(resolve=>setTimeout(resolve,200));await rm(fixture,{recursive:true,force:true});result.cleanup=true;await writeFile(join(evidence,'G-card-isolated-results.json'),JSON.stringify(result,null,2)+'\n');console.log(JSON.stringify(result,null,2))}
