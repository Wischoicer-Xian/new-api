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
// Self-test for the Wischoicer wallet recharge pure logic. Run with:
//   cd web/default && bun test src/features/wallet/lib/wischoicer-recharge.test.ts
//
// Covers every backend status path, the clientRequestId idempotency/recovery
// invariant, countdown/expiry, RMB formatting, and the no-leak projection.
import { afterEach, beforeEach, describe, expect, it } from 'bun:test'

import {
  WISCHOICER_PENDING_RECHARGE_KEY,
  WISCHOICER_RECHARGE_MAX_CENTS,
  WISCHOICER_RECHARGE_MIN_CENTS,
  WISCHOICER_RECHARGE_TIERS_CENTS,
  clearPendingRecharge,
  formatCentsAsYuan,
  formatCountdown,
  getCountdown,
  getWischoicerRechargePhase,
  isWischoicerRechargeTerminal,
  isWischoicerRechargeTier,
  newClientRequestId,
  readPendingRecharge,
  SAFE_RECHARGE_VIEW_KEYS,
  toSafeRechargeView,
  writePendingRecharge,
} from './wischoicer-recharge'

// Minimal localStorage shim so the persistence helpers can be exercised under Bun.
function installFakeLocalStorage() {
  const store = new Map<string, string>()
  const ls: Storage = {
    get length() {
      return store.size
    },
    clear: () => store.clear(),
    getItem: (k: string) => (store.has(k) ? (store.get(k) as string) : null),
    key: (i: number) => [...store.keys()][i] ?? null,
    removeItem: (k: string) => {
      store.delete(k)
    },
    setItem: (k: string, v: string) => {
      store.set(k, String(v))
    },
  }
  ;(globalThis as { window?: unknown }).window = globalThis.window ?? {}
  ;(globalThis as { window: Record<string, unknown> }).window.localStorage = ls
  ;(globalThis as { localStorage?: Storage }).localStorage = ls
}

describe('wischoicer recharge — tiers', () => {
  it('exposes exactly the locked RMB tiers and no ¥1 test tier', () => {
    expect([...WISCHOICER_RECHARGE_TIERS_CENTS]).toEqual([
      5000, 10000, 20000, 50000,
    ])
    expect(WISCHOICER_RECHARGE_TIERS_CENTS).not.toContain(100)
  })

  it('min/max bounds match the tier edges', () => {
    expect(WISCHOICER_RECHARGE_MIN_CENTS).toBe(5000)
    expect(WISCHOICER_RECHARGE_MAX_CENTS).toBe(50000)
  })

  it('accepts only the locked tiers and rejects tampered / test amounts', () => {
    for (const cents of WISCHOICER_RECHARGE_TIERS_CENTS) {
      expect(isWischoicerRechargeTier(cents)).toBe(true)
    }
    // ¥1 test amount (100) and arbitrary amounts must be rejected client-side.
    expect(isWischoicerRechargeTier(100)).toBe(false)
    expect(isWischoicerRechargeTier(3000)).toBe(false)
    expect(isWischoicerRechargeTier(0)).toBe(false)
    expect(isWischoicerRechargeTier(-5000)).toBe(false)
  })
})

describe('wischoicer recharge — status state machine', () => {
  it('maps every backend status to its user-facing phase', () => {
    expect(getWischoicerRechargePhase('PENDING_PAYMENT')).toBe('pending_payment')
    expect(getWischoicerRechargePhase('CREDITING')).toBe('crediting')
    expect(getWischoicerRechargePhase('SUCCESS')).toBe('success')
    expect(getWischoicerRechargePhase('CLOSED')).toBe('closed')
    expect(getWischoicerRechargePhase('CREDIT_FAILED')).toBe('credit_failed')
  })

  it('collapses unknown / missing status to a neutral phase (not success)', () => {
    expect(getWischoicerRechargePhase('SOMETHING_NEW')).toBe('unknown')
    expect(getWischoicerRechargePhase(undefined)).toBe('unknown')
    expect(getWischoicerRechargePhase(null)).toBe('unknown')
    expect(getWischoicerRechargePhase('')).toBe('unknown')
  })

  it('treats only success / closed / credit_failed as terminal', () => {
    expect(isWischoicerRechargeTerminal('success')).toBe(true)
    expect(isWischoicerRechargeTerminal('closed')).toBe(true)
    expect(isWischoicerRechargeTerminal('credit_failed')).toBe(true)
    // Pending, crediting and unknown must keep polling.
    expect(isWischoicerRechargeTerminal('pending_payment')).toBe(false)
    expect(isWischoicerRechargeTerminal('crediting')).toBe(false)
    expect(isWischoicerRechargeTerminal('unknown')).toBe(false)
  })
})

describe('wischoicer recharge — RMB formatting', () => {
  it('formats integer cents as yuan with two decimals', () => {
    expect(formatCentsAsYuan(5000)).toBe('50.00')
    expect(formatCentsAsYuan(10000)).toBe('100.00')
    expect(formatCentsAsYuan(20000)).toBe('200.00')
    expect(formatCentsAsYuan(50000)).toBe('500.00')
    expect(formatCentsAsYuan(100)).toBe('1.00')
  })

  it('never surfaces a raw quota integer', () => {
    // amountCents is always a money amount in cents, never the internal quota.
    expect(formatCentsAsYuan(5000)).not.toMatch(/^\d{6,}$/)
  })
})

describe('wischoicer recharge — clientRequestId idempotency & recovery', () => {
  beforeEach(installFakeLocalStorage)
  afterEach(() => {
    clearPendingRecharge()
  })

  it('mints a unique key per call within the 1–64 char server limit', () => {
    const a = newClientRequestId()
    const b = newClientRequestId()
    expect(a).not.toBe(b)
    expect(a.length).toBeGreaterThan(0)
    expect(a.length).toBeLessThanOrEqual(64)
    expect(b.length).toBeLessThanOrEqual(64)
  })

  it('persists and recovers the in-flight order so refresh does not re-create', () => {
    expect(readPendingRecharge()).toBeNull()
    const order = {
      orderNo: 'ORD123',
      clientRequestId: newClientRequestId(),
      amountCents: 10000,
      expireTime: 1_700_000_000,
    }
    writePendingRecharge(order)
    expect(readPendingRecharge()).toEqual(order)
  })

  it('clears the persisted order on terminal resolution', () => {
    writePendingRecharge({
      orderNo: 'ORD456',
      clientRequestId: 'rc-x',
      amountCents: 5000,
      expireTime: 1_700_000_000,
    })
    expect(readPendingRecharge()).not.toBeNull()
    clearPendingRecharge()
    expect(readPendingRecharge()).toBeNull()
  })

  it('ignores corrupt persisted data instead of crashing', () => {
    const ls = (globalThis as { window: { localStorage: Storage } }).window
      .localStorage
    ls.setItem(WISCHOICER_PENDING_RECHARGE_KEY, '{not json')
    expect(readPendingRecharge()).toBeNull()
    ls.setItem(
      WISCHOICER_PENDING_RECHARGE_KEY,
      JSON.stringify({ orderNo: 123, clientRequestId: 'x' })
    )
    expect(readPendingRecharge()).toBeNull()
  })
})

describe('wischoicer recharge — countdown / expiry', () => {
  it('counts down remaining seconds and flips to expired at zero', () => {
    const now = 1_700_000_000
    expect(getCountdown(now + 125, now)).toEqual({
      remainingSeconds: 125,
      expired: false,
    })
    expect(getCountdown(now + 1, now)).toEqual({
      remainingSeconds: 1,
      expired: false,
    })
    expect(getCountdown(now, now)).toEqual({
      remainingSeconds: 0,
      expired: true,
    })
    expect(getCountdown(now - 10, now)).toEqual({
      remainingSeconds: 0,
      expired: true,
    })
  })

  it('treats non-finite expireTime as already expired', () => {
    expect(getCountdown(Number.NaN, 1).expired).toBe(true)
  })

  it('formats seconds as MM:SS', () => {
    expect(formatCountdown(125)).toBe('02:05')
    expect(formatCountdown(59)).toBe('00:59')
    expect(formatCountdown(0)).toBe('00:00')
    expect(formatCountdown(-5)).toBe('00:00')
  })
})

describe('wischoicer recharge — no-leak projection', () => {
  it('keeps only browser-safe fields off a well-formed payload', () => {
    const view = toSafeRechargeView({
      orderNo: 'ORD1',
      amountCents: 5000,
      currency: 'CNY',
      status: 'PENDING_PAYMENT',
      codeUrl: 'weixin://wxpay/bizpayurl?pr=xxx',
      expireTime: 1_700_000_100,
    })
    expect(view).toEqual({
      orderNo: 'ORD1',
      amountCents: 5000,
      currency: 'CNY',
      status: 'PENDING_PAYMENT',
      codeUrl: 'weixin://wxpay/bizpayurl?pr=xxx',
      expireTime: 1_700_000_100,
    })
  })

  it('drops any internal field a downstream might ever leak', () => {
    const view = toSafeRechargeView({
      orderNo: 'ORD2',
      amountCents: 10000,
      currency: 'CNY',
      status: 'SUCCESS',
      // Everything below this line is forbidden in the browser.
      quota: 987654321,
      token: 'super-secret-internal-token',
      code: 'CREDIT_USER_UNAVAILABLE',
      httpStatus: 409,
      serviceName: 'wischoicer-billing',
      newApiUserId: 42,
      internalTrace: '0xaf',
    })
    // No key outside the safe allow-list may survive the projection.
    const allowed = new Set<string>(SAFE_RECHARGE_VIEW_KEYS)
    for (const key of Object.keys(view)) {
      expect(allowed.has(key)).toBe(true)
    }
    // Spot-check the most dangerous fields are absent.
    expect(view).not.toHaveProperty('quota')
    expect(view).not.toHaveProperty('token')
    expect(view).not.toHaveProperty('code')
    expect(view).not.toHaveProperty('serviceName')
    expect(view).not.toHaveProperty('newApiUserId')
    expect(view).not.toHaveProperty('httpStatus')
  })

  it('coerces missing/invalid fields to safe defaults instead of leaking undefined state', () => {
    const view = toSafeRechargeView({ status: 'PENDING_PAYMENT' })
    expect(view.orderNo).toBe('')
    expect(view.amountCents).toBe(0)
    expect(view.currency).toBe('CNY')
    expect(view.status).toBe('PENDING_PAYMENT')
    expect(view.codeUrl).toBeUndefined()
    expect(toSafeRechargeView(null).status).toBe('')
    expect(toSafeRechargeView(undefined).currency).toBe('CNY')
  })
})
