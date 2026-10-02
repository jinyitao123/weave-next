/** Small read-only production UI review: layout and retained reading position. */
import { createRequire } from 'node:module'
import { readFile, writeFile, mkdir } from 'node:fs/promises'
import { dirname, resolve, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import assert from 'node:assert/strict'
const evidence = dirname(fileURLToPath(import.meta.url))
const repo = resolve(evidence, '../../../../')
const { chromium } = createRequire(join(repo, 'workbench/apps/web/package.json'))('playwright')
const authUrl = (await readFile('<LOCAL_TEMP_PATH>, 'utf8')).match(/http:\/\/127\.0\.0\.1:13081\/\?token=[^\s]+/)?.[0]
assert.ok(authUrl)
await mkdir(evidence, { recursive: true })
const browser = await chromium.launch({ headless: true })
const page = await browser.newPage({ viewport: { width: 1440, height: 900 } })
const result = { date: new Date().toISOString(), provenance: '本地正式构建与真实历史会话；仅打开既有团队、成员记录和人审成果，未发送消息或执行业务操作', cases: [], reading: [], errors: [] }
page.on('pageerror', e => result.errors.push(e.message.replace(/token=[^\s&]+/g, 'token=REDACTED')))
const scene = () => page.locator('[data-weave-work-task]:visible')
const atTop = async () => { await scene().evaluate(el => { let p = el.parentElement; while (p && !/auto|scroll/.test(getComputedStyle(p).overflowY)) p = p.parentElement; if (p) p.scrollTop = 0 }); await page.waitForTimeout(300) }
const measure = async label => {
  const value = await scene().evaluate(el => {
    const rect = e => { const r=e.getBoundingClientRect(); return { x:r.x, y:r.y, width:r.width, height:r.height, right:r.right, bottom:r.bottom } }
    let owner = el.parentElement
    while (owner && !/auto|scroll/.test(getComputedStyle(owner).overflowY)) owner=owner.parentElement
    const tabs=el.querySelector('[role=tablist]')?.parentElement
    const member=el.querySelector('[data-weave-member-reader]')
    const rows=[...el.querySelectorAll('[class*="_memberList"] > button')].filter(row=>row.getClientRects().length>0).map(row => {
      const identity=row.querySelector('[class*="_memberIdentity"]')
      const name=identity?.querySelector('strong'), duty=identity?.querySelector('span'), status=row.querySelector('[class*="_memberState"]'), arrow=row.querySelector('[class*="_memberChevron"]')
      return { row:rect(row), name:name?{...rect(name),text:name.textContent}:null, duty:duty?rect(duty):null, status:status?rect(status):null, arrow:arrow?rect(arrow):null, grid:getComputedStyle(row).gridTemplateColumns }
    })
    const bodies=[...el.querySelectorAll('[role=log] > article > div')].map(b => ({ ...rect(b), maxWidth:getComputedStyle(b).maxWidth, font:getComputedStyle(b).fontSize, lineHeight:getComputedStyle(b).lineHeight }))
    const chapter=[...el.querySelectorAll('[role=log] h1,[role=log] h2,[role=log] h3')].find(h=>h.textContent?.includes('材料补齐后的验收工作范围'))
    return { viewport:{width:innerWidth,height:innerHeight}, documentWidth:document.documentElement.scrollWidth, scene:rect(el), sceneScrollWidth:el.scrollWidth, sceneMaxWidth:getComputedStyle(el).maxWidth, scroll:owner?{...rect(owner),top:owner.scrollTop,scrollHeight:owner.scrollHeight,clientHeight:owner.clientHeight}:null, tabs:tabs?{...rect(tabs),position:getComputedStyle(tabs).position}:null, member:member?rect(member):null, rows, bodies, chapter:chapter?{...rect(chapter),text:chapter.textContent}:null, expanded:[...el.querySelectorAll('[aria-expanded=true]')].map(b=>b.textContent) }
  })
  value.label=label
  assert.ok(value.documentWidth<=value.viewport.width, label+': no page horizontal overflow')
  assert.ok(value.sceneScrollWidth<=value.scene.width+1, label+': no scene horizontal overflow')
  assert.ok(value.scene.width<=960.5, label+': scene width limited to 960')
  for (const row of value.rows) {
    assert.ok(row.name.right<=row.status.x+1, label+': member name and status do not overlap')
    assert.ok(row.status.right<=row.arrow.x+1, label+': member status and arrow do not overlap')
    assert.ok(row.duty.right<=row.arrow.x+1, label+': member duty does not overlap arrow')
  }
  for (const body of value.bodies) assert.ok(body.width<=parseFloat(body.maxWidth)+1, label+': reading body fits 72ch')
  return value
}
const capture=async (label,file) => { const v=await measure(label); result.cases.push(v); await page.screenshot({path:join(evidence,file)}); return v }
try {
  result.phase='desktop-team'
  await page.goto(authUrl)
  await page.getByText('上线前本地知识库体验评审',{exact:true}).click()
  await page.waitForFunction(()=>document.querySelector('[data-conversation-scroll]')?.textContent.includes('已按确认方案完成交接'))
  await page.getByRole('button',{name:'打开工作现场',exact:true}).click()
  await scene().waitFor()
  await atTop()
  await capture('1440-team','J01-1440-team.png')
  result.phase='desktop-member'
  await scene().locator('[class*="_memberList"] > button').filter({has:page.getByText('体验验收负责人',{exact:true})}).click()
  await scene().locator('[data-weave-member-reader]').waitFor()
  const expand=scene().getByRole('button',{name:'展开本条记录',exact:true})
  if(await expand.count()) await expand.last().click()
  await atTop()
  const memberTop=await capture('1440-member','J02-1440-member.png')
  assert.ok(memberTop.member.y>=memberTop.tabs.bottom-1,'member begins below sticky tabs')

  result.phase='full-width'
  await scene().getByRole('button',{name:'全幅查看',exact:true}).click()
  await page.waitForTimeout(350)
  const full=await capture('1440-full-member','J03-1440-full-member.png')
  assert.ok(Math.abs(full.scene.width-960)<1,'full width uses 960px content cap')
  assert.ok(Math.abs((full.scene.x-full.scroll.x)-(full.scroll.right-full.scene.right))<4,'full scene content centered')
  for(const body of full.bodies) assert.ok(Math.abs(body.x-full.scene.x)<24,'full width public records share the left reading edge')
  await scene().getByRole('button',{name:'返回并排',exact:true}).click()
  await page.waitForTimeout(350)

  result.phase='reading-retention'
  await scene().evaluate(el => {
    const chapter=[...el.querySelectorAll('[role=log] h1,[role=log] h2,[role=log] h3')].find(h=>h.textContent?.includes('材料补齐后的验收工作范围'))
    if(!chapter) throw Error('known existing reading chapter missing')
    let owner=el.parentElement;while(owner&&!/auto|scroll/.test(getComputedStyle(owner).overflowY))owner=owner.parentElement
    owner.scrollTop+=chapter.getBoundingClientRect().y-420
  })
  await page.waitForTimeout(400)
  const before=await measure('reading-before');result.reading.push(before)
  assert.ok(before.chapter.y>before.tabs.bottom+40,'reading chapter clear of sticky tabs')
  await scene().getByRole('tab',{name:'成果',exact:true}).click()
  await scene().getByRole('tab',{name:'进展',exact:true}).click()
  await page.waitForTimeout(500)
  const returned=await measure('reading-tab-return');result.reading.push(returned)
  assert.ok(Math.abs(returned.scroll.top-before.scroll.top)<=1,'tab return retains scroll offset')
  assert.ok(Math.abs(returned.chapter.y-before.chapter.y)<=1,'tab return retains chapter position')
  await page.reload()
  await page.waitForFunction(()=>document.querySelector('[data-conversation-scroll]')?.textContent.includes('已按确认方案完成交接'))
  await page.getByRole('button',{name:'打开工作现场',exact:true}).click()
  await scene().locator('[data-weave-member-reader]').waitFor()
  await page.waitForTimeout(1200)
  const reloaded=await measure('reading-reloaded');result.reading.push(reloaded)
  assert.ok(Math.abs(reloaded.scroll.top-before.scroll.top)<=1,'reload retains scroll offset')
  assert.ok(Math.abs(reloaded.chapter.y-before.chapter.y)<=1,'reload retains chapter position')
  await page.screenshot({path:join(evidence,'J04-1440-reading-restored.png')})

  result.phase='narrow-member'
  await page.setViewportSize({width:390,height:844})
  await atTop()
  await capture('390-member','J05-390-member.png')
  await scene().getByRole('button',{name:'返回团队总览',exact:true}).click()
  await atTop()
  await capture('390-team','J06-390-team.png')

  result.phase='human-review-output'
  await page.setViewportSize({width:1440,height:900})
  await page.getByText('创建Workbench人审验收0905团队',{exact:true}).click()
  await page.waitForFunction(()=>document.querySelector('[data-conversation-scroll]')?.textContent.includes('已将待审稿提交给'))
  await scene().getByRole('tab',{name:'成果',exact:true}).click()
  await scene().locator('[data-deliverable-id]').first().waitFor()
  const output=scene().locator('[data-deliverable-id]').first()
  if(await output.getAttribute('open')===null) await output.locator('summary').first().click()
  await atTop()
  const finalOutput=await capture('1440-human-review-output','J07-1440-human-review-output.png')
  const summary=await output.locator('summary').first().evaluate(el=>({height:el.getBoundingClientRect().height,minHeight:getComputedStyle(el).minHeight,padding:getComputedStyle(el).padding}))
  assert.ok(summary.height>=64,'specialized deliverable summary spacing applies')
  result.output={summary,text:(await output.innerText()).slice(0,1000)}
  assert.match(result.output.text,/终审通过/)
  assert.deepEqual(result.errors,[])
  result.passed=true
} catch(e) {
  result.passed=false
  result.failure=String(e.message).replace(/token=[^\s&]+/g,'token=REDACTED')
  process.exitCode=1
} finally {
  await browser.close()
  await writeFile(join(evidence,'scene-layout-results.json'),JSON.stringify(result,null,2)+'\n')
  console.log(JSON.stringify({passed:result.passed,phase:result.phase,failure:result.failure,cases:result.cases.map(c=>({label:c.label,width:c.scene.width,rows:c.rows.length,bodyWidths:c.bodies.map(b=>b.width),scroll:c.scroll.top})),reading:result.reading.map(c=>({label:c.label,top:c.scroll.top,chapterY:c.chapter?.y})),errors:result.errors},null,2))
}
