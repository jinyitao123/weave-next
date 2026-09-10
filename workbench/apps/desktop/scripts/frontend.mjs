/** Package the Workbench profile's existing client graph and built frontend as local resources. */
import { globSync, readFileSync } from 'node:fs'
import { cp, mkdir, writeFile } from 'node:fs/promises'
import { createRequire } from 'node:module'
import { dirname, join } from 'node:path'
import { pathToFileURL } from 'node:url'
import { createHash } from 'node:crypto'

/**
 * Assemble the same profile layers as Workbench, retaining package-owned browser modules.
 * @param workspace - Workbench root with current host/client build outputs.
 * @param destination - exclusive native build resource directory.
 */
export async function packageFrontend(workspace, destination) {
  const layerNames = ['base', 'web-app', 'workbench-app']
  const layers = layerNames.map(name => join(workspace, 'packages/bundle', name))
  const resolver = createRequire(join(layers[1], 'package.json'))
  const { loadOverlayPatches, composeEntries } = await import(pathToFileURL(resolver.resolve('@deepseek-ai/dsh-app-boot')).href)
  const { bootInjections, orderByModuleGraph } = await import(pathToFileURL(resolver.resolve('@deepseek-ai/dsh-client-modules')).href)
  const manifests = new Map(globSync('packages/*/*/package.json', { cwd: workspace }).map(relative => {
    const path = join(workspace, relative)
    return [JSON.parse(readFileSync(path, 'utf8')).name, path]
  }))
  const composition = composeEntries(layers.map(layer => loadOverlayPatches('desktop build', join(layer, 'cordis.patch.yml'))))
  const plugins = []
  // The standalone fixture has no dynamic Host plugin-management endpoints.
  const omitted = new Set(['@deepseek-ai/dsh-cordis-client-runner', '@deepseek-ai/dsh-client-ui-cordis'])
  for (const entry of composition) {
    if (entry.disabled || omitted.has(entry.name) || !manifests.has(entry.name)) continue
    const path = manifests.get(entry.name)
    const pkg = JSON.parse(readFileSync(path, 'utf8'))
    const client = pkg.dsh?.client
    if (client?.platform !== 'web') continue
    const declared = pkg.exports['./client']
    const source = readFileSync(join(dirname(path), typeof declared === 'string' ? declared : declared.default), 'utf8')
    const url = `/plugins/${pkg.name}/client.js`
    const output = join(destination, url.slice(1))
    await mkdir(dirname(output), { recursive: true })
    await writeFile(output, source)
    plugins.push({ id: pkg.name, url, rev: createHash('sha256').update(source).digest('hex').slice(0, 16),
      ...(client.inject ? { inject: client.inject } : {}), ...(client.external ? { external: client.external } : {}),
      ...(client.immediately ? { immediately: true } : {}) })
  }
  const entries = orderByModuleGraph(plugins)
  const bootstrap = entries.filter(entry => entry.id === '@deepseek-ai/dsh-client-modules')
  const application = entries.filter(entry => entry.id !== '@deepseek-ai/dsh-client-modules')
  if (bootstrap.length !== 1) throw new Error('Workbench client bootstrap is missing')
  const graph = { rev: 'desktop-local', entries, batches: [
    { phase: 'bootstrap', url: '/bootstrap.js', rev: 'desktop-local', entries: bootstrap.map(entry => entry.id) },
    { phase: 'application', url: '/application.js', rev: 'desktop-local', entries: application.map(entry => entry.id) },
  ] }
  await cp(join(workspace, 'apps/web/dist'), destination, { recursive: true })
  await writeFile(join(destination, 'bootstrap.js'), bootstrap.map(entry => readFileSync(join(destination, entry.url.slice(1)), 'utf8')).join('\n;\n'))
  await writeFile(join(destination, 'application.js'), application.map(entry => readFileSync(join(destination, entry.url.slice(1)), 'utf8')).join('\n;\n'))
  const facade = bootInjections(graph).find(row => row.kind === 'script')
  if (!facade) throw new Error('Workbench module facade is missing')
  await writeFile(join(destination, 'boot.js'), `${facade.text}\nwindow.__DSH_BOOT__=${JSON.stringify(graph)};\n`)
  let html = readFileSync(join(destination, 'index.html'), 'utf8')
  html = html.replace('<head>', '<head>\n<script src="/boot.js"></script>\n<script src="/bootstrap.js"></script>')
  html = html.replace('<html lang="en">', '<html lang="zh-CN" data-weave-desktop>')
  await writeFile(join(destination, 'index.html'), html)
  await writeFile(join(destination, 'client-graph.json'), `${JSON.stringify({ ...graph, previewOmissions: [...omitted] }, null, 2)}\n`)
}
