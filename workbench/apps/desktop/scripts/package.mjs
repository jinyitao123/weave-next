/** Package the current host's native preview; signing and distribution are separate work. */
import { packager } from '@electron/packager'
import { fileURLToPath } from 'node:url'
import { buildIcon } from './icon.mjs'

const icon = await buildIcon()
const paths = await packager({ dir: fileURLToPath(new URL('../dist', import.meta.url)),
  out: fileURLToPath(new URL('../../../.dsh-build/desktop', import.meta.url)), name: 'Weave Workbench Preview',
  appBundleId: 'com.weave.workbench.preview', appVersion: '0.1.0', electronVersion: '44.2.0',
  icon, platform: process.platform, arch: process.arch, asar: true, overwrite: true, prune: false })
for (const path of paths) console.log(path)
