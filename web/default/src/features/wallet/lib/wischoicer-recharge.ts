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
// ============================================================================
// Wischoicer WeChat Native wallet recharge — pure browser-side logic
// ============================================================================
//
// Browser-safe mirror of the WIS-547 contract / WIS-550 wallet façade
// (`POST/GET /api/wallet/recharges*`). The new-api wallet backend is the single
// authority on amount tiers, ownership and status; this module only:
//   - mirrors the locked ¥50/100/200/500 tiers for UI rendering,
//   - maps the backend order status string to a small user-facing phase,
//   - mints/persists the per-order clientRequestId idempotency key so retries
//     and page refreshes never create a duplicate order,
//   - projects ONLY browser-safe fields off any API response (defense in depth:
//     quota / token / internal error codes / service names are dropped here and
//     can never reach the UI, even if a downstream ever leaked them).
//
// This file is dependency-free so it can be unit-tested directly with Bun.

/** Locked RMB tiers in cents (¥50 / ¥100 / ¥200 / ¥500). UI-only mirror of the
 * server whitelist (`common.WischoicerRechargeAllowedAmountCents`); the server
 * still enforces it, so tampering client-side is rejected upstream. ¥1 (100
 * cents) is a server-side test-only path and is intentionally absent here. */
export const WISCHOICER_RECHARGE_TIERS_CENTS = [
  5000, 10000, 20000, 50000,
] as const
export const WISCHOICER_RECHARGE_MIN_CENTS = 5000
export const WISCHOICER_RECHARGE_MAX_CENTS = 50000

/** Whether an amount (in cents) is one of the locked UI tiers. UI guard only —
 * the server authoritatively enforces the whitelist, so a tampered value is
 * rejected upstream regardless of this check. */
export function isWischoicerRechargeTier(amountCents: number): boolean {
  return WISCHOICER_RECHARGE_TIERS_CENTS.includes(
    amountCents as (typeof WISCHOICER_RECHARGE_TIERS_CENTS)[number]
  )
}

/** Backend order status strings projected by billing (`model.AggregateStatus`).
 * These are the only values the wallet façade surfaces as `status`. */
export const WischoicerRechargeStatus = {
  PendingPayment: 'PENDING_PAYMENT',
  Crediting: 'CREDITING',
  Success: 'SUCCESS',
  Closed: 'CLOSED',
  CreditFailed: 'CREDIT_FAILED',
} as const
export type WischoicerRechargeStatusValue =
  (typeof WischoicerRechargeStatus)[keyof typeof WischoicerRechargeStatus]

/** Small, stable user-facing phase the UI branches on. Unknown backend values
 * collapse to `'unknown'` and are treated as "still processing, keep polling"
 * with a neutral label — never as success or failure. */
export type WischoicerRechargePhase =
  | 'pending_payment'
  | 'crediting'
  | 'success'
  | 'closed'
  | 'credit_failed'
  | 'unknown'

export function getWischoicerRechargePhase(
  status: string | undefined | null
): WischoicerRechargePhase {
  switch (status) {
    case WischoicerRechargeStatus.PendingPayment:
      return 'pending_payment'
    case WischoicerRechargeStatus.Crediting:
      return 'crediting'
    case WischoicerRechargeStatus.Success:
      return 'success'
    case WischoicerRechargeStatus.Closed:
      return 'closed'
    case WischoicerRechargeStatus.CreditFailed:
      return 'credit_failed'
    default:
      return 'unknown'
  }
}

/** Terminal phases: no further transitions expected, stop polling. */
export function isWischoicerRechargeTerminal(
  phase: WischoicerRechargePhase
): boolean {
  return (
    phase === 'success' ||
    phase === 'closed' ||
    phase === 'credit_failed'
  )
}

// ---------------------------------------------------------------------------
// Amount formatting (RMB). amountCents is always an integer — never float.
// ---------------------------------------------------------------------------

/** Format integer cents as a yuan string with two decimals, e.g. 5000 -> "50.00". */
export function formatCentsAsYuan(cents: number): string {
  if (!Number.isFinite(cents)) return '0.00'
  return (cents / 100).toFixed(2)
}

// ---------------------------------------------------------------------------
// Idempotency: stable clientRequestId + pending-order recovery
// ---------------------------------------------------------------------------

/** localStorage key holding the single in-flight pending order, so a page
 * refresh recovers it instead of placing a duplicate. */
export const WISCHOICER_PENDING_RECHARGE_KEY = 'wischoicer_wallet_pending_recharge'

export interface PersistedPendingRecharge {
  clientRequestId: string
  amountCents: number
  /** Filled once the create-order response arrives. Absent while the response
   * is in flight — in that case mount recovery re-POSTs by clientRequestId
   * (idempotent re-fetch) instead of GETting an unknown orderNo. */
  orderNo?: string
  /** Unix seconds — filled from the create-order response's expireTime. */
  expireTime?: number
}

/** Mint a fresh idempotency key (1–64 chars, the server's limit). One per order
 * intent: reused across retries of the SAME intent, never across different
 * amounts (a different amount with the same key is rejected as ORDER_CONFLICT). */
export function newClientRequestId(): string {
  if (
    typeof crypto !== 'undefined' &&
    typeof crypto.randomUUID === 'function'
  ) {
    return crypto.randomUUID()
  }
  // Fallback when crypto.randomUUID is unavailable. Good enough as an
  // idempotency key (high-entropy, monotonic-ish); not used for secrets.
  return `rc-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`
}

function safeLocalStorage(): Storage | null {
  try {
    const g = globalThis as {
      localStorage?: Storage
      window?: { localStorage?: Storage }
    }
    // Prefer the global (the standard browser location, and an assignable slot
    // under test runners); fall back to window.localStorage for completeness.
    if (g.localStorage) return g.localStorage
    if (g.window?.localStorage) return g.window.localStorage
  } catch {
    /* localStorage may throw in private mode / sandboxed contexts */
  }
  return null
}

/** Read the persisted in-flight intent, or null if none / corrupt. Only
 * `clientRequestId` + `amountCents` are required; `orderNo` / `expireTime` are
 * absent until the create-order response arrives. */
export function readPendingRecharge(): PersistedPendingRecharge | null {
  const ls = safeLocalStorage()
  if (!ls) return null
  let raw: string | null = null
  try {
    raw = ls.getItem(WISCHOICER_PENDING_RECHARGE_KEY)
  } catch {
    return null
  }
  if (!raw) return null
  try {
    const parsed = JSON.parse(raw) as Record<string, unknown>
    if (
      typeof parsed.clientRequestId === 'string' &&
      parsed.clientRequestId.length > 0 &&
      typeof parsed.amountCents === 'number'
    ) {
      const out: PersistedPendingRecharge = {
        clientRequestId: parsed.clientRequestId,
        amountCents: parsed.amountCents,
      }
      if (typeof parsed.orderNo === 'string' && parsed.orderNo.length > 0) {
        out.orderNo = parsed.orderNo
      }
      if (typeof parsed.expireTime === 'number') {
        out.expireTime = parsed.expireTime
      }
      return out
    }
  } catch {
    /* corrupt JSON — treat as absent */
  }
  return null
}

/** Persist the in-flight intent so refresh/retry can recover it. The intent is
 * written BEFORE the create POST (with just clientRequestId + amountCents) and
 * upgraded with orderNo / expireTime once the response arrives. Silent on
 * failure (storage unavailable). */
export function writePendingRecharge(order: PersistedPendingRecharge): void {
  const ls = safeLocalStorage()
  if (!ls) return
  try {
    const payload: Record<string, unknown> = {
      clientRequestId: order.clientRequestId,
      amountCents: order.amountCents,
    }
    if (order.orderNo) {
      payload.orderNo = order.orderNo
    }
    if (typeof order.expireTime === 'number') {
      payload.expireTime = order.expireTime
    }
    ls.setItem(WISCHOICER_PENDING_RECHARGE_KEY, JSON.stringify(payload))
  } catch {
    /* ignore quota / private mode */
  }
}

/** Clear the persisted in-flight intent (on terminal success / close / cancel). */
export function clearPendingRecharge(): void {
  const ls = safeLocalStorage()
  if (!ls) return
  try {
    ls.removeItem(WISCHOICER_PENDING_RECHARGE_KEY)
  } catch {
    /* ignore */
  }
}

// ---------------------------------------------------------------------------
// Intent decisions (pure) — the core idempotency / recovery invariants
// ---------------------------------------------------------------------------

export interface CreateIntentDecision {
  clientRequestId: string
  /** true when an in-flight intent for the SAME amount is being reused, so the
   * server returns the original order instead of creating a duplicate (the
   * invariant that survives a dropped create response). */
  reused: boolean
}

/** Decide which clientRequestId to use for a create attempt. The same intent
 * (same amount, result unknown) MUST reuse the persisted idempotency key — that
 * is what stops a retry / page refresh from creating a duplicate order when the
 * first response was lost. A different amount (or no prior intent) starts a
 * fresh key. Pure on purpose: this decision is the thing the tests lock down. */
export function resolveCreateIntent(
  persisted: PersistedPendingRecharge | null,
  amountCents: number
): CreateIntentDecision {
  if (persisted && persisted.amountCents === amountCents) {
    return { clientRequestId: persisted.clientRequestId, reused: true }
  }
  return { clientRequestId: newClientRequestId(), reused: false }
}

export type RecoveryAction = 'none' | 'get' | 'repost'

/** Decide how to recover an in-flight intent on mount. With the orderNo known,
 * GET it; without it (create response was lost), re-POST idempotently with the
 * same clientRequestId to re-fetch the original order rather than abandon it. */
export function resolveRecoveryAction(
  persisted: PersistedPendingRecharge | null
): RecoveryAction {
  if (!persisted) return 'none'
  if (persisted.orderNo) return 'get'
  return 'repost'
}

// ---------------------------------------------------------------------------
// Countdown / expiry (expireTime is Unix seconds from the wallet façade)
// ---------------------------------------------------------------------------

export interface CountdownState {
  remainingSeconds: number
  expired: boolean
}

/** Compute remaining seconds and whether the order window has elapsed. */
export function getCountdown(
  expireTimeSeconds: number,
  nowSeconds: number
): CountdownState {
  if (!Number.isFinite(expireTimeSeconds) || !Number.isFinite(nowSeconds)) {
    return { remainingSeconds: 0, expired: true }
  }
  const remaining = Math.max(0, Math.floor(expireTimeSeconds - nowSeconds))
  return { remainingSeconds: remaining, expired: remaining <= 0 }
}

/** Format a positive second count as `MM:SS`. */
export function formatCountdown(totalSeconds: number): string {
  const s = Math.max(0, Math.floor(totalSeconds))
  const minutes = Math.floor(s / 60)
  const seconds = s % 60
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${pad(minutes)}:${pad(seconds)}`
}

// ---------------------------------------------------------------------------
// Safe projection: the only fields the browser may ever render
// ---------------------------------------------------------------------------

/** Browser-safe recharge view. Mirrors the WIS-550 `walletRechargeView`:
 * amount / orderNo / status / codeUrl / expireTime (+ optional paidTime for
 * forward compatibility if the façade exposes it). Never carries quota, token,
 * internal error codes or service names. */
export interface WischoicerWalletRechargeView {
  orderNo: string
  amountCents: number
  currency: string
  status: string
  codeUrl?: string
  expireTime?: number
  paidTime?: number
}

/** Project an arbitrary API payload down to browser-safe fields. Any extra key
 * the backend might ever return (quota, token, internal code, service name) is
 * dropped here and can never flow into the UI. */
export function toSafeRechargeView(raw: unknown): WischoicerWalletRechargeView {
  const o = (raw ?? {}) as Record<string, unknown>
  const view: WischoicerWalletRechargeView = {
    orderNo: typeof o.orderNo === 'string' ? o.orderNo : '',
    amountCents: typeof o.amountCents === 'number' ? o.amountCents : 0,
    currency: typeof o.currency === 'string' ? o.currency : 'CNY',
    status: typeof o.status === 'string' ? o.status : '',
  }
  if (typeof o.codeUrl === 'string' && o.codeUrl.length > 0) {
    view.codeUrl = o.codeUrl
  }
  if (typeof o.expireTime === 'number') {
    view.expireTime = o.expireTime
  }
  if (typeof o.paidTime === 'number') {
    view.paidTime = o.paidTime
  }
  return view
}

/** Project a whole history page through `toSafeRechargeView`, so the list path
 * gets the same defense in depth as the create/get path — a leaked quota / token
 * / internal field in any item is dropped before it reaches state. */
export function toSafeRechargeList(items: unknown): WischoicerWalletRechargeView[] {
  if (!Array.isArray(items)) return []
  return items.map((item) => toSafeRechargeView(item))
}

/** Known safe keys — used by tests to assert no internal field ever leaks. */
export const SAFE_RECHARGE_VIEW_KEYS = [
  'orderNo',
  'amountCents',
  'currency',
  'status',
  'codeUrl',
  'expireTime',
  'paidTime',
] as const
