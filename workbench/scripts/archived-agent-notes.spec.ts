import { execFileSync, spawnSync } from 'node:child_process'
import { copyFileSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import {
  extendArchiveManifest,
  gitBlobHash,
  parseArchiveManifest,
  renderArchiveManifest,
  validateArchiveArtifacts,
  validateArchiveManifestExtension,
  type ArchiveManifest,
} from './archived-agent-notes.ts'
import { isArchivedAgentNotePath } from './repo-files.ts'

function fixture(): Map<string, Buffer> {
  const base = '2026-07-26-example'
  const source = Buffer.from(`# Agent Note: Example\n\nStatus: implemented\nArchived: 2026-07-26\n\nEnglish | [中文](${base}.zh.md)\n\n## Problem\n\nExample.\n`)
  const zh = Buffer.from(`# Agent Note: 示例\n\nStatus: implemented\nArchived: 2026-07-26\n\n[English](${base}.md) | 中文\n\n## 问题\n\n示例。\n`)
  const meta = Buffer.from(`${base}.md: ${gitBlobHash(source)}\n${base}.zh.md: ${gitBlobHash(zh)}\n`)
  return new Map([
    [`process/${base}.md`, source],
    [`process/${base}.zh.md`, zh],
    [`process/${base}.i18n.yaml`, meta],
  ])
}

describe('archived Agent Notes', () => {
  it('reads the committed archive from a nested workspace and rejects replaced seals', () => {
    const repo = mkdtempSync(resolve(tmpdir(), 'weave-archive-git-'))
    const workspace = resolve(repo, 'workbench')
    const archive = resolve(workspace, '.agents/notes/archived')
    const manifest = resolve(archive, 'manifest.json')
    const artifacts = fixture()
    const run = () => spawnSync(process.execPath, [
      resolve(import.meta.dirname, '../node_modules/tsx/dist/cli.mjs'),
      resolve(workspace, 'scripts/verify-archived-agent-notes.ts'),
    ], { cwd: workspace, encoding: 'utf8' })
    try {
      mkdirSync(resolve(workspace, 'scripts'), { recursive: true })
      writeFileSync(resolve(workspace, 'package.json'), '{"type":"module"}\n')
      for (const name of ['verify-archived-agent-notes.ts', 'archived-agent-notes.ts', 'agent-note-tree.ts']) {
        copyFileSync(resolve(import.meta.dirname, name), resolve(workspace, 'scripts', name))
      }
      for (const kind of ['feature', 'bug-fix', 'simplification', 'architecture', 'process', 'testing']) {
        mkdirSync(resolve(archive, kind), { recursive: true })
      }
      writeFileSync(resolve(archive, 'AGENTS.md'), '# Frozen archive\n')
      for (const [name, bytes] of artifacts) {
        mkdirSync(dirname(resolve(archive, name)), { recursive: true })
        writeFileSync(resolve(archive, name), bytes)
      }
      const sealed = extendArchiveManifest({ version: 1, files: {} }, artifacts)
      writeFileSync(manifest, renderArchiveManifest(sealed.files))
      execFileSync('git', ['init', '--quiet', repo])
      execFileSync('git', ['-C', repo, 'add', '.'])
      execFileSync('git', ['-C', repo, '-c', 'user.name=Archive Test', '-c', 'user.email=archive@example.invalid', 'commit', '--quiet', '-m', 'Seal fixture'])
      const accepted = run()
      expect(accepted.status, accepted.stderr).toBe(0)
      writeFileSync(manifest, renderArchiveManifest({}))
      const rejected = run()
      expect(rejected.status).toBe(1)
      expect(rejected.stderr).toContain('sealed manifest entry is missing')
    } finally {
      rmSync(repo, { recursive: true, force: true })
    }
  })

  it('recognizes archived paths with POSIX and Windows separators', () => {
    expect(isArchivedAgentNotePath('.agents/notes/archived/process/example.md')).toBe(true)
    expect(isArchivedAgentNotePath('.agents\\notes\\archived\\process\\example.md')).toBe(true)
    expect(isArchivedAgentNotePath('.agents/notes/implemented/process/example.md')).toBe(false)
  })

  it('accepts one complete implemented triplet with matching archive metadata', () => {
    expect(validateArchiveArtifacts(fixture())).toEqual([])
  })

  it('rejects incomplete triplets and invalid archive headers', () => {
    const artifacts = fixture()
    artifacts.delete('process/2026-07-26-example.i18n.yaml')
    artifacts.set(
      'process/2026-07-26-example.md',
      Buffer.from('# Agent Note: Example\n\nStatus: proposed\nArchived: yesterday\n'),
    )
    expect(validateArchiveArtifacts(artifacts).join('\n')).toMatch(/incomplete archived triplet/)
  })

  it('extends the manifest without permitting a sealed change or removal', () => {
    const artifacts = fixture()
    const empty: ArchiveManifest = { version: 1, files: {} }
    const first = extendArchiveManifest(empty, artifacts)
    expect(first.errors).toEqual([])
    expect(first.added).toHaveLength(3)

    const sealed: ArchiveManifest = { version: 1, files: first.files }
    const changed = new Map(artifacts)
    changed.set('process/2026-07-26-example.md', Buffer.from('changed'))
    expect(extendArchiveManifest(sealed, changed).errors).toEqual([
      'process/2026-07-26-example.md: sealed content hash changed',
    ])
    changed.delete('process/2026-07-26-example.zh.md')
    expect(extendArchiveManifest(sealed, changed).errors).toContain(
      'process/2026-07-26-example.zh.md: sealed artifact is missing',
    )
  })

  it('rejects replacing manifest seals alongside changed archive content', () => {
    const artifacts = fixture()
    const initial = extendArchiveManifest({ version: 1, files: {} }, artifacts)
    const baseline: ArchiveManifest = { version: 1, files: initial.files }
    const path = 'process/2026-07-26-example.md'
    const changedArtifacts = new Map(artifacts)
    changedArtifacts.set(path, Buffer.from('changed'))
    const replacement = extendArchiveManifest({ version: 1, files: {} }, changedArtifacts)
    const current: ArchiveManifest = { version: 1, files: replacement.files }

    expect(extendArchiveManifest(current, changedArtifacts).errors).toEqual([])
    expect(validateArchiveManifestExtension(baseline, current)).toEqual([
      `${path}: sealed manifest hash changed`,
    ])
    const removed: ArchiveManifest = {
      version: 1,
      files: Object.fromEntries(Object.entries(current.files).filter(([candidate]) => candidate !== path)),
    }
    expect(validateArchiveManifestExtension(baseline, removed)).toContain(
      `${path}: sealed manifest entry is missing`,
    )
  })

  it('round-trips the deterministic manifest schema', () => {
    const content = renderArchiveManifest({ 'process/z.md': `sha256:${'a'.repeat(64)}` })
    expect(parseArchiveManifest(content)).toEqual({
      version: 1,
      files: { 'process/z.md': `sha256:${'a'.repeat(64)}` },
    })
  })
})
