/** Isolated component QA only. Every image is synthetic, never a real Workbench run. */
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
const fixture = await mkdtemp(join(web, '.preview-qa-'))
const results = { provenance: '隔离预览测试数据，非真实运行', component: 'workbench/packages/client/ui-weave/src/client/DeliverablePreview.tsx', cases: [], errors: [], cleanup: false }
let server
let browser
try {
  await writeFile(join(fixture, 'index.html'), '<!doctype html><html lang="zh-CN"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>隔离预览测试数据，非真实运行</title></head><body><div id="root"></div><script type="module" src="/main.tsx"></script></body></html>')
  await writeFile(join(fixture, 'drawing.svg'), '<svg xmlns="http://www.w3.org/2000/svg" width="1200" height="600" viewBox="0 0 1200 600"><rect width="1200" height="600" fill="#fff"/><g fill="none" stroke="#475569" stroke-width="2"><rect x="40" y="90" width="1120" height="460" rx="12"/><path d="M320 90v460M760 90v460M40 320h1120"/><circle cx="540" cy="200" r="70"/><path d="M930 150v100m-50-50h100"/></g><g font-family="sans-serif" fill="#1e293b" font-size="24"><text x="40" y="48">Isolated SVG preview fixture — not a real run</text><text x="70" y="140">A / LEFT EDGE</text><text x="800" y="140">B / RIGHT EDGE</text><text x="70" y="500">1200 × 600</text><text x="840" y="500">END OF DRAWING</text></g></svg>')
  await writeFile(join(fixture, 'main.tsx'), `import React, { useState } from 'react'
import { createRoot } from 'react-dom/client'
import { DeliverablePreview } from '../../../packages/client/ui-weave/src/client/DeliverablePreview.tsx'
import { deliverablePreviewLabels, zh } from '../../../packages/client/ui-weave/src/client/preview-locales.ts'
import '../../../packages/client/ui-theme/src/styles/base.css'
import '../../../packages/client/ui-theme/src/styles/design-platform.css'
import '../../../packages/client/ui-theme/src/styles/scrollbar.css'
import './fixture.css'
const labels = deliverablePreviewLabels(key => zh[key])
const csv = '成员,当前工作,状态,备注,阶段,更新,来源,结论\\n体验负责人,整理评审材料,已完成,"保留逗号，一与二",汇总,09:30,隔离数据,等待复核\\n信息架构评审员,核对信息层级,进行中,"第一行\\n第二行",评审,09:32,隔离数据,尚未结束\\n视觉评审员,核对图标与展开,已完成,细线图标,评审,09:35,隔离数据,通过'
const tsv = Array.from({length:201}, (_,row) => Array.from({length:41}, (_,column) => row === 0 ? '列 ' + (column + 1) : row + ':' + column).join('\\t')).join('\\n')
function App() {
 const [kind, setKind] = useState('csv')
 const [file, setFile] = useState(1)
 const [shown, setShown] = useState(true)
 return <><header className="fixtureNotice">隔离预览测试数据，非真实运行</header><main><h1>成果预览 · 隔离验收</h1><nav aria-label="测试样本"><button onClick={() => setKind('csv')}>CSV 样本</button><button onClick={() => setKind('tsv')}>TSV 限额样本</button><button onClick={() => setKind('svg')}>SVG 样本</button><button onClick={() => setFile(v => v + 1)}>切换文件</button><button onClick={() => setShown(v => !v)}>折叠测试区</button></nav><div hidden={!shown}><DeliverablePreview contentType={kind === 'csv' ? 'text/csv' : kind === 'tsv' ? 'text/tab-separated-values' : 'image/svg+xml'} body={kind === 'csv' ? csv : kind === 'tsv' ? tsv : ''} title={kind === 'svg' ? '隔离示意图' : kind === 'csv' ? '隔离成员记录' : '隔离限额表格'} imageUrl={'/drawing.svg?file=' + file} download={{url:'/drawing.svg?download=' + file, label:'下载文件',notice:''}} labels={labels} /></div></main></>
}
createRoot(document.getElementById('root')!).render(<App />)
`)
  await writeFile(join(fixture, 'fixture.css'), 'body{margin:0;font-family:var(--dsw-font-family);background:var(--dsw-alias-bg-base);color:var(--dsw-alias-label-primary)}.fixtureNotice{padding:12px 16px;background:var(--dsw-alias-bg-module-platform);font-size:13px;line-height:20px;border-bottom:1px solid var(--dsw-alias-border-l2)}main{max-width:1000px;margin:24px auto;padding:0 16px}h1{font-size:22px;line-height:1.4;margin:0 0 16px}nav{display:flex;flex-wrap:wrap;gap:8px;margin-bottom:20px}nav button{font:inherit;font-size:12px;padding:6px 10px;border:1px solid var(--dsw-alias-border-l2);border-radius:6px;background:var(--dsw-alias-bg-base);color:inherit}')
  server = await createServer({ configFile: false, root: fixture, plugins: [react()], cacheDir: join(fixture, 'cache'), server: { host: '127.0.0.1', port: 0, fs: { allow: [join(repo, 'workbench')] } }, resolve: { dedupe: ['react', 'react-dom'] } })
  await server.listen()
  const address = server.httpServer.address()
  const url = 'http://127.0.0.1:' + address.port
  browser = await chromium.launch({ headless: true })
  const page = await browser.newPage()
  page.on('pageerror', error => results.errors.push(error.message))
  for (const width of [1440, 390]) {
    await page.setViewportSize({width, height:width === 1440 ? 1000 : 1000})
    await page.goto(url)
    await page.getByRole('table').waitFor()
    for (const kind of ['csv', 'tsv', 'svg']) {
      await page.getByRole('button', {name:kind === 'csv' ? 'CSV 样本' : kind === 'tsv' ? 'TSV 限额样本' : 'SVG 样本', exact:true}).click()
      const region = page.getByRole('region', {name:kind === 'svg' ? '隔离示意图' : '表格预览'})
      if (kind === 'svg') await page.getByRole('button', {name:'放大图纸', exact:true}).waitFor({state:'visible'})
      if (kind === 'svg') await page.waitForFunction(() => document.querySelector('img')?.complete && document.querySelector('img')?.naturalWidth > 0)
      const baseline = await region.evaluate(el => ({width:el.clientWidth, scrollWidth:el.scrollWidth, height:el.clientHeight, scrollHeight:el.scrollHeight, pageWidth:document.documentElement.scrollWidth, viewport:innerWidth}))
      assert.ok(baseline.pageWidth <= width, kind + ' page-level overflow')
      const result = {width, kind, baseline, downloadVisible: await page.getByRole('link', {name:'下载文件'}).isVisible()}
      if (kind !== 'svg') {
        result.rows = await page.getByRole('row').count()
        result.columns = await page.getByRole('row').first().getByRole('cell').count()
        if (kind === 'tsv') {
          assert.equal(result.rows, 200)
          assert.equal(result.columns, 40)
          assert.equal(await page.getByRole('status').textContent(), '当前展示前 200 行、前 40 列，完整内容请下载文件。')
        }
      }
      await page.screenshot({path:join(evidence, 'F-' + kind + '-' + width + '.png'), fullPage:true})
      if (kind === 'svg') {
        for(let step=0;step<4;step++) await page.getByRole('button', {name:'放大图纸',exact:true}).click()
        assert.equal(await page.getByRole('status',{name:'缩放比例'}).textContent(),'200%')
        result.zoom = await region.evaluate(el => ({clientWidth:el.clientWidth,scrollWidth:el.scrollWidth}))
        assert.ok(result.zoom.scrollWidth > result.zoom.clientWidth)
        await region.hover()
        await page.mouse.wheel(10000, 0)
        await page.waitForFunction(() => { const el = document.querySelector('[role=region]'); return el && el.scrollLeft >= el.scrollWidth - el.clientWidth - 1 })
        result.scrollLeft = await region.evaluate(el => el.scrollLeft)
        assert.ok(result.scrollLeft > 0)
        await region.focus()
        await page.keyboard.press('ArrowLeft')
        await page.waitForFunction(previous => document.querySelector('[role=region]').scrollLeft < previous, result.scrollLeft)
        result.keyboardScroll = true
        await page.waitForTimeout(300) // Allow native keyboard scrolling to settle before recording the position.
        result.scrollLeft = await region.evaluate(el => el.scrollLeft)
        await page.screenshot({path:join(evidence, 'F-svg-' + width + '-zoom200-right.png'),fullPage:true})
        await page.getByRole('button',{name:'折叠测试区',exact:true}).click()
        await page.getByRole('button',{name:'折叠测试区',exact:true}).click()
        assert.equal(await page.getByRole('status',{name:'缩放比例'}).textContent(),'200%')
        assert.equal(await region.evaluate(el => el.scrollLeft),result.scrollLeft)
        await page.getByRole('button',{name:'适应宽度',exact:true}).click()
        assert.equal(await page.getByRole('status',{name:'缩放比例'}).textContent(),'100%')
        await page.getByRole('button',{name:'放大图纸',exact:true}).click()
        await page.getByRole('button',{name:'切换文件',exact:true}).click()
        assert.equal(await page.getByRole('status',{name:'缩放比例'}).textContent(),'100%')
        result.fileReset = true
        result.hiddenStatePreserved = true
        result.horizontalInput = 'native horizontal wheel'
      } else if (baseline.scrollWidth > baseline.width) {
        await region.hover()
        await page.mouse.wheel(10000, 0)
        await page.waitForFunction(() => { const el = document.querySelector('[role=region]'); return el && el.scrollLeft >= el.scrollWidth - el.clientWidth - 1 })
        result.scrollLeft = await region.evaluate(el => el.scrollLeft)
        assert.ok(result.scrollLeft > 0)
        await region.focus()
        await page.keyboard.press('ArrowLeft')
        await page.waitForFunction(previous => document.querySelector('[role=region]').scrollLeft < previous, result.scrollLeft)
        result.keyboardScroll = true
        await page.waitForTimeout(300) // Allow native keyboard scrolling to settle before recording the position.
        result.scrollLeft = await region.evaluate(el => el.scrollLeft)
        await page.screenshot({path:join(evidence, 'F-' + kind + '-' + width + '-right.png'),fullPage:true})
      }
      results.cases.push(result)
    }
  }
  assert.deepEqual(results.errors, [])
  results.result = 'passed'
} catch (error) {
  results.result = 'failed'
  results.failure = error.stack
  process.exitCode = 1
} finally {
  await browser?.close()
  await server?.close()
  await new Promise(resolve => setTimeout(resolve, 200)) // Let Vite finish pending optimizer cache writes.
  await rm(fixture, {recursive:true,force:true})
  results.cleanup = true
  await writeFile(join(evidence,'F-preview-isolated-results.json'),JSON.stringify(results,null,2)+'\n')
  console.log(JSON.stringify(results,null,2))
}
