/** Exclusive, atomic preference storage for the desktop main process. */
import { closeSync, fsyncSync, mkdirSync, openSync, readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs'
import { dirname } from 'node:path'
import { randomUUID } from 'node:crypto'
import type { ConnectionPreferenceStore } from './connection-selection.js'

/** One desktop instance owns this file; successful rename is the commit point. */
export class FilePreferences implements ConnectionPreferenceStore {
  /** @param path - file inside the exclusively locked application data directory. */
  constructor(private readonly path: string) {}

  /** @returns persisted JSON, or null only when the file does not exist. */
  read(): string | null {
    try { return readFileSync(this.path, 'utf8') } catch (error) {
      if (error instanceof Error && 'code' in error && error.code === 'ENOENT') return null
      throw error
    }
  }

  /** @param value - non-sensitive JSON; errors before rename preserve the previous file. */
  write(value: string): void {
    mkdirSync(dirname(this.path), { recursive: true, mode: 0o700 })
    const temporary = `${this.path}.${randomUUID()}.tmp`
    let descriptor: number | undefined
    let committed = false
    try {
      descriptor = openSync(temporary, 'wx', 0o600)
      writeFileSync(descriptor, value)
      fsyncSync(descriptor)
      closeSync(descriptor)
      descriptor = undefined
      renameSync(temporary, this.path)
      committed = true
    } finally {
      if (descriptor !== undefined) closeSync(descriptor)
      if (!committed) rmSync(temporary, { force: true })
    }
  }
}
