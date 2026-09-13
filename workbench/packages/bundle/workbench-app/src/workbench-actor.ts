import { createHash } from 'node:crypto'

/** Derive one opaque Weave actor from the authenticated Workbench browser session. */
export function workbenchActor(request: Request): string {
  const cookie = request.headers.get('cookie')?.trim() ?? ''
  const source = cookie === '' ? 'workbench-browser-without-cookie' : cookie
  return createHash('sha256').update(source).digest('hex')
}
