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
// Hook-level tests for useWischoicerRecharge — the wiring the pure-logic tests
// cannot reach: that createOrder persists the intent BEFORE the POST and reuses
// the same clientRequestId across a lost-response retry, and that a SUCCESS
// learned from mount recovery fires onRechargeSuccess exactly once.
//
// Run with: cd web/default && bun test src/features/wallet/hooks/use-wischoicer-recharge.test.ts
import { resolve } from 'node:path'
import { afterEach, beforeEach, describe, expect, it, mock } from 'bun:test'
import { GlobalRegistrator } from '@happy-dom/global-registrator'
import { act, createElement } from 'react'
import { createRoot } from 'react-dom/client'

// Provide a DOM (window / document / localStorage / ...) for React + the hook.
GlobalRegistrator.register()
// Tell React this is an act environment so effects flush inside act().
;(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT =
  true

const PENDING_KEY = 'wischoicer_wallet_pending_recharge'
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

beforeEach(() => {
  globalThis.localStorage.clear()
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
    // First create: response drops (network reject). Second: succeeds.
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
    const { result, unmount } = renderHook(() => useWischoicerRecharge(onSuccess))

    // First attempt fails — but the intent MUST already be persisted so a retry
    // recovers instead of creating a duplicate order.
    let ok = true
    await act(async () => {
      ok = await result.current.createOrder(5000)
    })
    expect(ok).toBe(false)
    const afterFail = readPending()
    expect(afterFail).not.toBeNull()
    expect(afterFail?.amountCents).toBe(5000)
    expect(typeof afterFail?.clientRequestId).toBe('string')
    const firstId = afterFail?.clientRequestId

    // Retry: same intent must reuse the SAME clientRequestId (no duplicate).
    await act(async () => {
      ok = await result.current.createOrder(5000)
    })
    expect(ok).toBe(true)
    const retryReq = api.create.mock.calls[1][0] as { clientRequestId: string }
    expect(retryReq.clientRequestId).toBe(firstId)

    unmount()
  })
})

describe('useWischoicerRecharge — recovery balance refresh', () => {
  it('fires onRechargeSuccess exactly once when mount recovery finds SUCCESS', async () => {
    const now = Math.floor(Date.now() / 1000)
    // Seed a resolved order so recovery GETs it on mount.
    globalThis.localStorage.setItem(
      PENDING_KEY,
      JSON.stringify({
        clientRequestId: 'rc-recovered',
        amountCents: 5000,
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

    // Recovery runs async on mount; let it settle.
    await act(async () => {
      await new Promise((r) => setTimeout(r, 50))
    })

    expect(onSuccess).toHaveBeenCalledTimes(1)
    // The intent is cleared on terminal success.
    expect(readPending()).toBeNull()

    unmount()
  })
})
