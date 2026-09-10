/** Build a native application resource directory without publishing a workspace package. */
import { build } from 'tsdown'
import { cp, mkdir, rm, writeFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'
import { packageFrontend } from './frontend.mjs'

const directory = fileURLToPath(new URL('..', import.meta.url))
const outDir = join(directory, 'dist')
await rm(outDir, { recursive: true, force: true })
await mkdir(outDir, { recursive: true })
for (const [name, format, platform] of [['main', 'esm', 'node'], ['preload', 'cjs', 'node'], ['renderer', 'esm', 'browser']]) {
  await build({ config: false, entry: { [name]: join(directory, 'src', `${name}.ts`) }, outDir, format,
    platform, target: 'es2023', clean: false, dts: false, sourcemap: true, deps: { neverBundle: ['electron'] },
    outExtensions: () => ({ js: format === 'cjs' ? '.cjs' : '.js' }) })
}
await cp(join(directory, 'static'), outDir, { recursive: true })
await packageFrontend(fileURLToPath(new URL('../../..', import.meta.url)), join(outDir, 'workbench'))
await writeFile(join(outDir, 'package.json'), `${JSON.stringify({ name: 'weave-workbench-preview',
  productName: 'Weave Workbench Preview', version: '0.1.0', private: true, type: 'module', main: 'main.js',
  description: 'Isolated native Workbench connection preview', author: 'Weave', license: 'MIT' }, null, 2)}\n`)
