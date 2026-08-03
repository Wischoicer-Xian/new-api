/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { afterEach, beforeEach, describe, expect, it, mock } from 'bun:test'
// Hook-level tests for useWischoicerRecharge — the wiring pure-logic tests
// cannot reach: idempotent reuse across a lost-response retry, terminal routing
// of an immediate SUCCESS on create/retry, mount-recovery balance refresh, and
// the A→B cross-user guard that stops one user's intent being re-POSTed under
// another user's session.
//
// Run with: cd web/default && bun test src/features/wallet/hooks/use-wischoicer-recharge.test.ts
import { resolve } from 'node:path'

import { GlobalRegistrator } from '@happy-dom/global-registrator'
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'

// Provide a DOM (window / document / localStorage / ...) for React + the hook.
GlobalRegistrator.register()
// Tell React this is an act environment so effects flush inside act().
;(
  globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }
).IS_REACT_ACT_ENVIRONMENT = true

const PENDING_KEY = 'wischoicer_wallet_pending_recharge'
const UID_KEY = 'uid'
const apiPath = resolve(import.meta.dir, '../api.ts')

// Mock the wallet api module the hook imports. Each test configures the returns.
const api = {
  create: mock<(req: unknown) => Promise<unknown>>(),
  get: mock<(orderNo: string) => Promise<unknown>>(),
  available: mock<() => Promise<unknown>>(),
}

await mock.module(apiPath, () => ({
  createWischoicerRecharge: (req: unknown) => api.create(req),
  getWischoicerRecharge: (orderNo: string) => api.get(orderNo),
  isWischoicerRechargeAvailable: () => api.available(),
}))

const { useWischoicerRecharge } = await import('./use-wischoicer-recharge')

interface Harness<T> {
  result: { current: T }
  unmount: () => void
}

function renderHook<T>(fn: () => T): Harness<T> {
  const container = document.createElement('div')
  document.body.appendChild(container)
  const result = { current: undefined as unknown as T }
  const Comp = () => {
    result.current = fn()
    return null
  }
  const root = createRoot(container)
  act(() => {
    root.render(createElement(Comp))
  })
  return {
    result,
    unmount: () =>
      act(() => {
        root.unmount()
      }),
  }
}

function readPending(): Record<string, unknown> | null {
  const raw = globalThis.localStorage.getItem(PENDING_KEY)
  return raw ? (JSON.parse(raw) as Record<string, unknown>) : null
}

function setUid(uid: string) {
  globalThis.localStorage.setItem(UID_KEY, uid)
}

// Default to an authenticated user; tests that need a different user override.
beforeEach(() => {
  globalThis.localStorage.clear()
  setUid('U1')
  api.create.mockClear()
  api.get.mockClear()
  api.available.mockClear()
})

afterEach(() => {
  globalThis.localStorage.clear()
})

describe('useWischoicerRecharge — idempotency wiring', () => {
  it('persists the intent before the POST and reuses the id on a lost-response retry', async () => {
    api.available.mockResolvedValue(true)
    api.create.mockRejectedValueOnce(new Error('network down'))
    api.create.mockResolvedValue({
      success: true,
      data: {
        orderNo: 'ORD1',
        amountCents: 5000,
        currency: 'CNY',
        status: 'PENDING_PAYMENT',
        expireTime: Math.floor(Date.now() / 1000) + 600,
      },
    })

    const onSuccess = mock()
    const { result, unmount } = renderHook(() =>
      useWischoicerRecharge(onSuccess)
    )

    let ok = true
    await act(async () => {
      ok = await result.current.createOrder(5000)
    })
    expect(ok).toBe(false)
    const afterFail = readPending()
    expect(afterFail).not.toBeNull()
    expect(afterFail?.amountCents).toBe(5000)
    expect(afterFail?.uid).toBe('U1')
    const firstId = afterFail?.clientRequestId

    await act(async () => {
      ok = await result.current.createOrder(5000)
    })
    expect(ok).toBe(true)
    const retryReq = api.create.mock.calls[1][0] as { clientRequestId: string }
    expect(retryReq.clientRequestId).toBe(firstId)

    unmount()
  })

  it('fires onRechargeSuccess exactly once when a create/retry returns an immediate SUCCESS', async () => {
    const now = Math.floor(Date.now() / 1000)
    // A prior create dropped (no orderNo); retrying with the same id gets back an
    // order that has already been paid/credited.
    globalThis.localStorage.setItem(
      PENDING_KEY,
      JSON.stringify({
        clientRequestId: 'rc-immediate',
        amountCents: 5000,
        uid: 'U1',
      })
    )
    api.available.mockResolvedValue(true)
    api.create.mockResolvedValue({
      success: true,
      data: {
        orderNo: 'ORD-PAID',
        amountCents: 5000,
        currency: 'CNY',
        status: 'SUCCESS',
        paidTime: now,
      },
    })

    const onSuccess = mock()
    const { result, unmount } = renderHook(() =>
      useWischoicerRecharge(onSuccess)
    )

    let ok = false
    await act(async () => {
      ok = await result.current.createOrder(5000)
    })
    expect(ok).toBe(true)
    // Reused the persisted id (no duplicate order), and terminal routing fired.
    const req = api.create.mock.calls[0][0] as { clientRequestId: string }
    expect(req.clientRequestId).toBe('rc-immediate')
    expect(onSuccess).toHaveBeenCalledTimes(1)
    expect(readPending()).toBeNull()

    unmount()
  })
})

describe('useWischoicerRecharge — recovery & cross-user ownership', () => {
  it('fires onRechargeSuccess exactly once when mount recovery finds SUCCESS', async () => {
    const now = Math.floor(Date.now() / 1000)
    globalThis.localStorage.setItem(
      PENDING_KEY,
      JSON.stringify({
        clientRequestId: 'rc-recovered',
        amountCents: 5000,
        uid: 'U1',
        orderNo: 'ORD-S',
        expireTime: now + 100,
      })
    )
    api.available.mockResolvedValue(true)
    api.get.mockResolvedValue({
      success: true,
      data: {
        orderNo: 'ORD-S',
        amountCents: 5000,
        currency: 'CNY',
        status: 'SUCCESS',
        paidTime: now,
      },
    })

    const onSuccess = mock()
    const { unmount } = renderHook(() => useWischoicerRecharge(onSuccess))

    await act(async () => {
      await new Promise((r) => setTimeout(r, 50))
    })

    expect(onSuccess).toHaveBeenCalledTimes(1)
    expect(readPending()).toBeNull()
    unmount()
  })

  it('never re-POSTs another user leftover intent on an A→B switch', async () => {
    // User B is now logged in on the same browser, but the intent belongs to A.
    setUid('B')
    globalThis.localStorage.setItem(
      PENDING_KEY,
      JSON.stringify({ clientRequestId: 'rc-A', amountCents: 5000, uid: 'A' })
    )
    api.available.mockResolvedValue(true)

    const onSuccess = mock()
    const { unmount } = renderHook(() => useWischoicerRecharge(onSuccess))

    await act(async () => {
      await new Promise((r) => setTimeout(r, 50))
    })

    // A's intent must NOT be recovered — neither re-POSTed nor GETted under B.
    expect(api.create).not.toHaveBeenCalled()
    expect(api.get).not.toHaveBeenCalled()
    expect(onSuccess).not.toHaveBeenCalled()
    // The foreign intent is discarded so it can't bite a later recovery either.
    expect(readPending()).toBeNull()

    unmount()
  })
})
