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
import i18next from 'i18next'
import { useCallback, useEffect, useRef, useState } from 'react'
import { toast } from 'sonner'

import {
  createWischoicerRecharge,
  getWischoicerRecharge,
  isWischoicerRechargeAvailable,
} from '../api'
import {
  clearPendingRecharge,
  getCountdown,
  getWischoicerRechargePhase,
  isWischoicerRechargeTerminal,
  isWischoicerRechargeTier,
  newClientRequestId,
  readPendingRecharge,
  toSafeRechargeView,
  writePendingRecharge,
  type WischoicerRechargePhase,
  type WischoicerWalletRechargeView,
} from '../lib/wischoicer-recharge'

// Poll the order status this often while waiting for payment / crediting.
const POLL_INTERVAL_MS = 3000

export interface UseWischoicerRechargeResult {
  /** null while probing; true once the wallet façade is mounted, false otherwise. */
  available: boolean | null
  /** The active (in-flight or just-resolved) order, or null. */
  order: WischoicerWalletRechargeView | null
  phase: WischoicerRechargePhase
  /** Remaining seconds on the QR pay window (0 when none / expired). */
  remainingSeconds: number
  /** Client-side QR window has elapsed (order not yet confirmed paid). */
  expired: boolean
  creating: boolean
  /** Safe, user-facing error message (never an internal code). */
  error: string | null
  /** Create an order for a tier (amountCents). Returns true on success. */
  createOrder: (amountCents: number) => Promise<boolean>
  /** Drop the active order so the user can pick another tier. */
  reset: () => void
  clearError: () => void
}

/**
 * Drives the WeChat Native wallet recharge lifecycle against the new-api wallet
 * façade. Handles idempotent create (stable per-order clientRequestId), page
 * refresh recovery, QR countdown/expiry, status polling across every backend
 * phase, and balance refresh on success. Never surfaces quota / token / internal
 * codes — the server already maps those to safe messages, and `toSafeRechargeView`
 * strips anything else.
 */
export function useWischoicerRecharge(
  onRechargeSuccess?: () => void
): UseWischoicerRechargeResult {
  const [available, setAvailable] = useState<boolean | null>(null)
  const [order, setOrder] = useState<WischoicerWalletRechargeView | null>(null)
  const [phase, setPhase] = useState<WischoicerRechargePhase>('pending_payment')
  const [remainingSeconds, setRemainingSeconds] = useState(0)
  const [expired, setExpired] = useState(false)
  const [creating, setCreating] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Latest order without re-subscribing the polling effect on every poll tick.
  const orderRef = useRef<WischoicerWalletRechargeView | null>(null)
  // clientRequestId for the current order intent: reused across retries of the
  // same intent, regenerated only when the user starts a new intent.
  const clientRequestIdRef = useRef<string>(newClientRequestId())
  const mountedRef = useRef(true)
  const successFiredRef = useRef(false)

  useEffect(() => {
    mountedRef.current = true
    return () => {
      mountedRef.current = false
    }
  }, [])

  const applyOrder = useCallback((view: WischoicerWalletRechargeView) => {
    // Defense in depth: only ever keep the browser-safe projection in state.
    const safe = toSafeRechargeView(view)
    const nextPhase = getWischoicerRechargePhase(safe.status)
    orderRef.current = safe
    setOrder(safe)
    setPhase(nextPhase)
    return { safe, nextPhase }
  }, [])

  const tickCountdown = useCallback((nowSeconds: number) => {
    const current = orderRef.current
    if (!current?.expireTime) {
      return
    }
    const cd = getCountdown(current.expireTime, nowSeconds)
    setRemainingSeconds(cd.remainingSeconds)
    setExpired(cd.expired)
  }, [])

  const pollOnce = useCallback(
    async (orderNo: string) => {
      try {
        const res = await getWischoicerRecharge(orderNo)
        if (!mountedRef.current) return
        if (!res?.success || !res.data) return

        const { nextPhase } = applyOrder(res.data)
        tickCountdown(Math.floor(Date.now() / 1000))
        if (nextPhase === 'success') {
          clearPendingRecharge()
          if (!successFiredRef.current) {
            successFiredRef.current = true
            onRechargeSuccess?.()
            toast.success(i18next.t('Recharge successful'))
          }
        } else if (isWischoicerRechargeTerminal(nextPhase)) {
          clearPendingRecharge()
        }
      } catch {
        // Transient fetch failure — keep polling; never surface internal details.
      }
    },
    [applyOrder, onRechargeSuccess, tickCountdown]
  )

  // Mount: probe availability + recover an in-flight pending order.
  useEffect(() => {
    let cancelled = false
    void (async () => {
      const ok = await isWischoicerRechargeAvailable()
      if (cancelled || !mountedRef.current) return
      setAvailable(ok)
      if (!ok) return

      const pending = readPendingRecharge()
      if (!pending) return
      try {
        const res = await getWischoicerRecharge(pending.orderNo)
        if (cancelled || !mountedRef.current) return
        if (res?.success && res.data) {
          const { nextPhase } = applyOrder(res.data)
          // Resume only a still-live order; otherwise drop the stale row.
          if (isWischoicerRechargeTerminal(nextPhase)) {
            clearPendingRecharge()
          } else {
            clientRequestIdRef.current = pending.clientRequestId
            tickCountdown(Math.floor(Date.now() / 1000))
          }
        } else {
          clearPendingRecharge()
        }
      } catch {
        clearPendingRecharge()
      }
    })()
    return () => {
      cancelled = true
    }
  }, [applyOrder, tickCountdown])

  // Polling + 1s countdown clock while an order is live and non-terminal.
  // Depends on orderNo + phase only, so it does not tear down on every poll.
  useEffect(() => {
    const orderNo = order?.orderNo
    if (!orderNo) return
    if (isWischoicerRechargeTerminal(phase)) return

    let cancelled = false
    const clock = setInterval(() => {
      if (!cancelled && mountedRef.current) {
        tickCountdown(Math.floor(Date.now() / 1000))
      }
    }, 1000)
    const poll = setInterval(() => {
      if (!cancelled && mountedRef.current) {
        void pollOnce(orderNo)
      }
    }, POLL_INTERVAL_MS)

    return () => {
      cancelled = true
      clearInterval(clock)
      clearInterval(poll)
    }
  }, [order?.orderNo, phase, pollOnce, tickCountdown])

  const createOrder = useCallback(
    async (amountCents: number) => {
      setError(null)

      // UI guard only — the server still authoritatively enforces the tiers.
      if (!isWischoicerRechargeTier(amountCents)) {
        const msg = i18next.t('Recharge amount is not available')
        setError(msg)
        toast.error(msg)
        return false
      }

      // New intent -> fresh idempotency key; drop any prior in-flight order.
      clientRequestIdRef.current = newClientRequestId()
      clearPendingRecharge()
      orderRef.current = null
      setOrder(null)
      setPhase('pending_payment')
      setExpired(false)
      setRemainingSeconds(0)
      successFiredRef.current = false

      setCreating(true)
      try {
        const res = await createWischoicerRecharge({
          clientRequestId: clientRequestIdRef.current,
          amountCents,
        })
        if (!mountedRef.current) return false

        if (!res?.success || !res.data) {
          // Server already mapped any internal code to a safe message.
          const msg =
            res?.message || i18next.t('Recharge failed, please try again')
          setError(msg)
          toast.error(msg)
          return false
        }

        const { safe } = applyOrder(res.data)
        if (safe.expireTime) {
          writePendingRecharge({
            orderNo: safe.orderNo,
            clientRequestId: clientRequestIdRef.current,
            amountCents,
            expireTime: safe.expireTime,
          })
        }
        tickCountdown(Math.floor(Date.now() / 1000))
        return true
      } catch {
        if (!mountedRef.current) return false
        const msg = i18next.t('Recharge failed, please try again')
        setError(msg)
        toast.error(msg)
        return false
      } finally {
        if (mountedRef.current) setCreating(false)
      }
    },
    [applyOrder, tickCountdown]
  )

  const reset = useCallback(() => {
    clearPendingRecharge()
    clientRequestIdRef.current = newClientRequestId()
    successFiredRef.current = false
    orderRef.current = null
    setOrder(null)
    setPhase('pending_payment')
    setRemainingSeconds(0)
    setExpired(false)
    setError(null)
  }, [])

  const clearError = useCallback(() => setError(null), [])

  return {
    available,
    order,
    phase,
    remainingSeconds,
    expired,
    creating,
    error,
    createOrder,
    reset,
    clearError,
  }
}
