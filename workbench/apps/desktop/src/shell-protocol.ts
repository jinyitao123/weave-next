/** Narrow IPC messages shared only by the trusted local shell and its preload. */

/** Current local UI state; no credentials or business receipts cross this bridge. */
export interface ShellState {
  readonly selectedOrigin: string
  readonly instanceId: string | null
  readonly activeOrigin: string | null
  readonly busy: boolean
  readonly settingsOpen: boolean
  readonly notice: string
  readonly detail: string
  readonly fatal: boolean
  readonly services: readonly { name: string; origin: string }[]
}

/** Local shell commands; the main process validates every received value and sender. */
export type ShellCommand = { action: 'connect'; origin: string }
  | { action: 'cancel' | 'disconnect' | 'settings' | 'back' | 'reload' }

/** Preload capability available exclusively to the packaged local page. */
export interface ShellBridge {
  /** @returns initial local state after the renderer attaches its event listener. */
  snapshot(): Promise<ShellState>
  /** @param command - one validated shell action, never an arbitrary IPC channel. */
  command(command: ShellCommand): Promise<void>
  /** @param listener - local state subscriber; returns its unsubscription callback. */
  subscribe(listener: (state: ShellState) => void): () => void
}
