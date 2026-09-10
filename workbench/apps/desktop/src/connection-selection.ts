/** Service selection and stale-response checks, independent of Host transport and credentials. */

import type { Branded } from '@deepseek-ai/dsh-brand'

/** Canonical public service origin. */
export type ServiceOrigin = Branded<'desktop-service-origin'>
/** Identity supplied by a trusted service-description adapter. */
export type ServiceInstanceId = Branded<'desktop-service-instance-id'>

/** Non-sensitive preference; a first-launch default has no verified instance yet. */
export interface ConnectionProfile {
  readonly origin: ServiceOrigin
  readonly expectedInstanceId: ServiceInstanceId | null
}

/** Verified identity used by adapters; this is not an HTTP response schema. */
export interface ServiceIdentity {
  readonly origin: ServiceOrigin
  readonly expectedInstanceId: ServiceInstanceId
}

/** A client-local selection snapshot; never sent as a server authorization proof. */
export interface ConnectionScope extends ServiceIdentity {
  readonly generation: number
}

/** The desktop main process supplies an atomic, synchronous preference store. */
export interface ConnectionPreferenceStore {
  /** @returns saved JSON, or null only when no preference exists. */
  read(): string | null
  /**
   * Replace the preference atomically; a throw must leave the previous value intact.
   * @param value - versioned JSON containing only the selected origin and instance.
   */
  write(value: string): void
}

/** Adapters own HTTP parsing, instance verification, credentials, and session partitioning. */
export interface ConnectionAdapters {
  /**
   * Resolve the identity of exactly this origin, rejecting redirects to another origin.
   * @param origin - selected public service origin.
   * @param signal - abort signal for this selection attempt.
   * @returns verified service identity, without credentials.
   */
  describe(origin: ServiceOrigin, signal: AbortSignal): Promise<ServiceIdentity>
  /**
   * Authenticate to the described instance; keep credentials out of this module.
   * @param service - identity which authentication must target and revalidate.
   * @param signal - abort signal for this selection attempt.
   * @returns the actual authenticated service identity.
   */
  authenticate(service: ServiceIdentity, signal: AbortSignal): Promise<ServiceIdentity>
}

/** Explicit configuration; plain HTTP is restricted to opt-in loopback fixtures. */
export interface ConnectionSelectionOptions {
  readonly defaultOrigin: string
  readonly allowLoopbackHttp?: boolean
}

/**
 * Validate an origin supplied by configuration, persisted JSON, or an adapter.
 * @param value - absolute origin without credentials, path, query, or fragment.
 * @param allowLoopbackHttp - permit HTTP only on loopback addresses for local tests.
 * @returns a canonical origin; invalid input throws without changing selection.
 */
export function serviceOrigin(value: string, allowLoopbackHttp = false): ServiceOrigin {
  const url = new URL(value)
  const loopback = url.hostname === 'localhost' || url.hostname === '127.0.0.1' || url.hostname === '[::1]'
  if (url.username || url.password || url.pathname !== '/' || url.search || url.hash
    || (url.protocol !== 'https:' && !(allowLoopbackHttp && loopback && url.protocol === 'http:'))) {
    throw new Error('Expected an HTTPS origin without credentials, path, query, or fragment')
  }
  return url.origin as ServiceOrigin
}

/**
 * Validate a service instance identifier from an adapter or persisted JSON.
 * @param value - opaque, nonempty identifier.
 * @returns a branded identity; empty identifiers throw.
 */
export function serviceInstanceId(value: string): ServiceInstanceId {
  if (!value.trim() || value !== value.trim()) throw new Error('Expected a nonempty service instance ID')
  return value as ServiceInstanceId
}

function readProfile(json: string, allowLoopbackHttp: boolean): ConnectionProfile {
  const value: unknown = JSON.parse(json)
  if (typeof value !== 'object' || value === null || Array.isArray(value)
    || !('version' in value) || value.version !== 1
    || !('origin' in value) || typeof value.origin !== 'string'
    || !('expectedInstanceId' in value) || typeof value.expectedInstanceId !== 'string'
    || Object.keys(value).some(key => !['version', 'origin', 'expectedInstanceId'].includes(key))) {
    throw new Error('Invalid connection preference; restore or explicitly replace it')
  }
  return Object.freeze({
    origin: serviceOrigin(value.origin, allowLoopbackHttp),
    expectedInstanceId: serviceInstanceId(value.expectedInstanceId),
  })
}

async function abortable<T>(work: () => Promise<T>, signal: AbortSignal): Promise<T> {
  signal.throwIfAborted()
  let onAbort: () => void = () => {}
  const aborted = new Promise<never>((_resolve, reject) => {
    onAbort = () => { reject(signal.reason instanceof Error ? signal.reason : new Error('Connection selection cancelled')) }
    signal.addEventListener('abort', onAbort, { once: true })
  })
  try {
    return await Promise.race([work(), aborted])
  } finally {
    signal.removeEventListener('abort', onAbort)
  }
}

/** Selection owner. Network results can update the UI only through a current scope. */
export class ConnectionSelection {
  #selected: ConnectionProfile
  #active: ServiceIdentity | null = null
  #attempt: AbortController | null = null
  #generation = 0
  #closed = false
  #scopes = new WeakSet<ConnectionScope>()

  /**
   * Load a saved preference; malformed data throws instead of falling back to the default.
   * @param store - exclusive preference storage owned by this desktop process.
   * @param options - first-launch default and explicit loopback policy.
   */
  constructor(
    private readonly store: ConnectionPreferenceStore,
    private readonly options: ConnectionSelectionOptions,
  ) {
    const saved = store.read()
    this.#selected = saved === null
      ? Object.freeze({ origin: serviceOrigin(options.defaultOrigin, options.allowLoopbackHttp), expectedInstanceId: null })
      : readProfile(saved, options.allowLoopbackHttp ?? false)
  }

  /** @returns selected preference, independent of authentication and connection readiness. */
  get selected(): ConnectionProfile { return this.#selected }

  /** @returns whether a candidate is being checked or authenticated. */
  get switching(): boolean { return this.#attempt !== null }

  /**
   * Check and authenticate a candidate, then persist and activate it as one local commit.
   * Failure or cancellation preserves the previous preference. A detected replacement
   * at the active origin also clears readiness until that identity is resolved.
   * All earlier response scopes become stale when an attempt starts, including A→B→A.
   * @param origin - candidate origin; the selected origin is explicit at call sites.
   * @param adapters - trusted identity and authentication adapters.
   * @returns the new active scope; rejects on adapter, persistence, identity, or abort failure.
   */
  async select(origin: string, adapters: ConnectionAdapters): Promise<ConnectionScope> {
    if (this.#closed) throw new Error('Connection selection is closed')
    const canonical = serviceOrigin(origin, this.options.allowLoopbackHttp)
    this.cancel()
    const attempt = new AbortController()
    this.#attempt = attempt
    this.#generation++
    const expected = canonical === this.#selected.origin ? this.#selected.expectedInstanceId : null
    try {
      const description = await abortable(() => adapters.describe(canonical, attempt.signal), attempt.signal)
      attempt.signal.throwIfAborted()
      if (description.origin !== canonical || (expected !== null && description.expectedInstanceId !== expected)) {
        if (this.#active?.origin === canonical) this.#active = null
        throw new Error('Service instance changed; connection preference was preserved')
      }
      const service = Object.freeze({ origin: canonical, expectedInstanceId: description.expectedInstanceId })
      const authenticated = await abortable(() => adapters.authenticate(service, attempt.signal), attempt.signal)
      attempt.signal.throwIfAborted()
      if (authenticated.origin !== service.origin || authenticated.expectedInstanceId !== service.expectedInstanceId) {
        if (this.#active?.origin === canonical) this.#active = null
        throw new Error('Authentication targeted a different service instance')
      }
      this.store.write(JSON.stringify({ version: 1, ...service }))
      this.#selected = service
      this.#active = service
      this.#attempt = null
      return this.capture()
    } finally {
      if (this.#attempt === attempt) this.#attempt = null
    }
  }

  /** Cancel only the local selection attempt; no server operation is revoked. */
  cancel(): void {
    const attempt = this.#attempt
    this.#attempt = null
    if (attempt) {
      this.#generation++
      attempt.abort(new Error('Connection selection cancelled'))
    }
  }

  /** Clear local authentication/readiness and invalidate responses; keep the saved origin. */
  disconnect(): void {
    this.cancel()
    this.#generation++
    this.#active = null
  }

  /** Close this owner; adapters remain responsible for completing aborted network teardown. */
  close(): void {
    this.disconnect()
    this.#closed = true
  }

  /** @returns an immutable active scope; throws while switching, disconnected, or closed. */
  capture(): ConnectionScope {
    if (this.#closed || this.#attempt || !this.#active) throw new Error('No active connection is ready')
    const scope = Object.freeze({ ...this.#active, generation: this.#generation })
    this.#scopes.add(scope)
    return scope
  }

  /**
   * Check whether a response may update the active UI. Retain old operation receipts elsewhere.
   * @param scope - scope captured before a read, subscription, refresh, or upload.
   * @returns true only for the current, ready selection in this owner.
   */
  isCurrent(scope: ConnectionScope): boolean {
    return this.#scopes.has(scope) && !this.#closed && !this.#attempt && this.#active !== null
      && scope.generation === this.#generation && scope.origin === this.#active.origin
      && scope.expectedInstanceId === this.#active.expectedInstanceId
  }
}
