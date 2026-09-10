/** Render the native Weave asset and macOS iconset into the generated application directory. */
import { chromium } from 'playwright'
import { mkdir, readFile, rm } from 'node:fs/promises'
import { execFile } from 'node:child_process'
import { promisify } from 'node:util'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

/** @returns the native icon path; packaging currently targets this macOS preview. */
export async function buildIcon() {
  const directory = fileURLToPath(new URL('../dist', import.meta.url))
  const browser = await chromium.launch({ headless: true })
  try {
    const page = await browser.newPage({ viewport: { width: 1024, height: 1024 } })
    await page.setContent(`<html><style>html,body{margin:0;background:transparent}</style>${await readFile(join(directory, 'icon.svg'), 'utf8')}</html>`)
    await page.screenshot({ path: join(directory, 'icon.png'), omitBackground: true })
  } finally { await browser.close() }
  if (process.platform !== 'darwin') return join(directory, 'icon.png')
  const iconset = join(directory, 'AppIcon.iconset')
  await mkdir(iconset, { recursive: true })
  try {
    const run = promisify(execFile)
    for (const size of [16, 32, 128, 256, 512]) {
      for (const scale of [1, 2]) {
        await run('sips', ['-z', String(size * scale), String(size * scale), join(directory, 'icon.png'),
          '--out', join(iconset, `icon_${size}x${size}${scale === 2 ? '@2x' : ''}.png`)])
      }
    }
    const path = join(directory, 'AppIcon.icns')
    await run('iconutil', ['-c', 'icns', '-o', path, iconset])
    return path
  } finally { await rm(iconset, { recursive: true, force: true }) }
}
