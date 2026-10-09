// Copies the Vite build into the Go embed directory, keeping its placeholder.
import { cpSync, existsSync, readdirSync, rmSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = dirname(dirname(fileURLToPath(import.meta.url)))
const source = join(root, 'dist')
const target = join(root, '..', '..', 'internal', 'app', 'adminui', 'dist')
if (!existsSync(join(source, 'index.html'))) throw new Error('web/admin/dist/index.html is missing; run vite build first')
for (const entry of readdirSync(target)) if (entry !== '.gitkeep') rmSync(join(target, entry), { recursive: true, force: true })
cpSync(source, target, { recursive: true })
console.log(`admin console copied to ${target}`)
