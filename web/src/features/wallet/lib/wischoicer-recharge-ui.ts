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
import type { TFunction } from 'i18next'

import type { StatusVariant } from '@/components/status-badge'

import type { WischoicerRechargePhase } from './wischoicer-recharge'

export interface WischoicerPhaseStatus {
  label: string
  variant: StatusVariant
  /** Optional longer user-facing hint shown under the status. */
  hint?: string
}

/**
 * Map a user-facing recharge phase to a localized label + badge variant + hint.
 * Every backend phase (and the neutral "unknown" fallback) gets a plain-language
 * user message; none of these strings reveal quota, tokens, internal codes or
 * service names.
 */
export function getWischoicerPhaseStatus(
  phase: WischoicerRechargePhase,
  t: TFunction
): WischoicerPhaseStatus {
  switch (phase) {
    case 'pending_payment':
      return {
        label: t('Awaiting payment'),
        variant: 'warning',
        hint: t('Scan the QR code with WeChat to pay'),
      }
    case 'crediting':
      return {
        label: t('Payment received, crediting'),
        variant: 'info',
        hint: t('Your balance is being updated, please wait'),
      }
    case 'success':
      return {
        label: t('Recharge successful'),
        variant: 'success',
      }
    case 'closed':
      return {
        label: t('Order closed'),
        variant: 'neutral',
        hint: t('This order has been closed. Please start a new recharge.'),
      }
    case 'credit_failed':
      return {
        label: t('Recharge failed'),
        variant: 'danger',
        hint: t(
          'Payment was received but crediting failed. Please contact support.'
        ),
      }
    default:
      return {
        label: t('Processing'),
        variant: 'info',
        hint: t('Updating order status, please wait'),
      }
  }
}
