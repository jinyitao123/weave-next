import { describe, expect, it } from 'vitest'
import { parsePatch } from './environments'

describe('parsePatch', () => {
  it('splits files and classifies lines', () => {
    const patch = [
      'diff --git a/app.txt b/app.txt',
      'index 1111111..2222222 100644',
      '--- a/app.txt',
      '+++ b/app.txt',
      '@@ -1 +1,2 @@',
      ' one',
      '+more',
      'diff --git a/logo.png b/logo.png',
      'new file mode 100644',
      'GIT binary patch',
      'literal 3',
      'Kcmb=2#',
      '',
    ].join('\n')
    const files = parsePatch(patch)
    expect(files.map((file) => file.path)).toEqual(['app.txt', 'logo.png'])
    expect(files[0].lines.map((line) => line.kind)).toEqual(['hunk', 'ctx', 'add'])
    expect(files[0].lines[2].text).toBe('more')
    expect(files[1].binary).toBe(true)
    expect(files[1].lines).toEqual([])
  })
})
