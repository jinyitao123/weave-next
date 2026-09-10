/** Sandboxed preload for the trusted application page only. */
import { contextBridge, ipcRenderer } from 'electron'
import type { ShellBridge, ShellState } from './shell-protocol.js'

const bridge: ShellBridge = {
  snapshot: () => ipcRenderer.invoke('desktop:snapshot') as Promise<ShellState>,
  command: command => ipcRenderer.invoke('desktop:command', command) as Promise<void>,
  subscribe: (listener) => {
    const receive = (_event: unknown, state: ShellState) => { listener(state) }
    ipcRenderer.on('desktop:state', receive)
    return () => { ipcRenderer.removeListener('desktop:state', receive) }
  },
}
contextBridge.exposeInMainWorld('workbenchDesktop', bridge)
