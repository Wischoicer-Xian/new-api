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
import { Check, Copy, Loader2, RefreshCw } from 'lucide-react'
import { QRCodeSVG } from 'qrcode.react'
import type { ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { useCopyToClipboard } from '@/hooks/use-copy-to-clipboard'

import {
  formatCentsAsYuan,
  formatCountdown,
  type WischoicerRechargePhase,
  type WischoicerWalletRechargeView,
} from '../../lib/wischoicer-recharge'
import { getWischoicerPhaseStatus } from '../../lib/wischoicer-recharge-ui'

interface WischoicerRechargeQrDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  order: WischoicerWalletRechargeView | null
  phase: WischoicerRechargePhase
  remainingSeconds: number
  expired: boolean
  onReset: () => void
}

export function WischoicerRechargeQrDialog(
  props: WischoicerRechargeQrDialogProps
) {
  const { t } = useTranslation()
  const { copyToClipboard, copiedText } = useCopyToClipboard({ notify: false })

  const status = getWischoicerPhaseStatus(props.phase, t)
  const orderNo = props.order?.orderNo ?? ''
  const awaitingPayment =
    props.phase === 'pending_payment' || props.phase === 'unknown'
  const showQr = awaitingPayment && !!props.order?.codeUrl && !props.expired

  const renderStatusBody = (): ReactNode => {
    if (showQr && props.order?.codeUrl) {
      return (
        <>
          <div className='rounded-xl border bg-white p-3'>
            <QRCodeSVG value={props.order.codeUrl} size={200} />
          </div>
          <div className='text-center'>
            <div className='text-muted-foreground text-xs'>
              {t('Scan with WeChat to pay')}
            </div>
            <div className='text-foreground mt-1 text-2xl font-semibold tabular-nums'>
              {formatCountdown(props.remainingSeconds)}
            </div>
            <div className='text-muted-foreground text-xs'>
              {t('QR code expires after the countdown')}
            </div>
          </div>
        </>
      )
    }
    if (props.expired && awaitingPayment) {
      return (
        <div className='text-center'>
          <div className='text-foreground text-sm font-medium'>
            {t('QR code expired')}
          </div>
          <div className='text-muted-foreground mt-1 text-xs'>
            {t('The payment window has closed. Start a new recharge.')}
          </div>
        </div>
      )
    }
    if (props.phase === 'crediting' || props.phase === 'unknown') {
      return (
        <div className='flex flex-col items-center gap-2 py-4'>
          <Loader2 className='text-muted-foreground h-8 w-8 animate-spin' />
          <div className='text-muted-foreground text-sm'>
            {status.hint ?? t('Processing')}
          </div>
        </div>
      )
    }
    if (props.phase === 'success') {
      return (
        <div className='flex flex-col items-center gap-2 py-4'>
          <div className='bg-success/10 flex h-12 w-12 items-center justify-center rounded-full'>
            <Check className='h-6 w-6 text-green-600' />
          </div>
          <div className='text-foreground text-sm font-medium'>
            {t('Recharge successful')}
          </div>
        </div>
      )
    }
    return (
      <div className='text-muted-foreground py-4 text-center text-sm'>
        {status.hint ?? status.label}
      </div>
    )
  }

  const handleReset = () => {
    props.onReset()
    props.onOpenChange(false)
  }

  const showRechargeAgain =
    (props.expired && props.phase !== 'success') || props.phase === 'closed'

  return (
    <Dialog open={props.open} onOpenChange={props.onOpenChange}>
      <DialogContent className='sm:max-w-md'>
        <DialogHeader>
          <DialogTitle>{t('WeChat Recharge')}</DialogTitle>
          <DialogDescription>
            {props.order
              ? `${t('Amount')}: ¥${formatCentsAsYuan(props.order.amountCents)}`
              : t('Scan the QR code with WeChat to pay')}
          </DialogDescription>
        </DialogHeader>

        <div className='flex flex-col items-center gap-4 py-2'>
          <StatusBadge
            label={status.label}
            variant={status.variant}
            showDot
            copyable={false}
          />

          {renderStatusBody()}

          {orderNo ? (
            <div className='bg-muted/50 flex w-full items-center justify-between gap-2 rounded-lg px-3 py-2'>
              <div className='min-w-0'>
                <div className='text-muted-foreground text-xs'>
                  {t('Order number')}
                </div>
                <code className='text-foreground truncate font-mono text-xs'>
                  {orderNo}
                </code>
              </div>
              <Button
                variant='ghost'
                size='sm'
                className='h-7 shrink-0'
                onClick={() => copyToClipboard(orderNo)}
              >
                {copiedText === orderNo ? (
                  <Check className='h-3.5 w-3.5' />
                ) : (
                  <Copy className='h-3.5 w-3.5' />
                )}
                {t('Copy')}
              </Button>
            </div>
          ) : null}
        </div>

        <DialogFooter>
          {showRechargeAgain ? (
            <Button onClick={handleReset}>
              <RefreshCw className='h-4 w-4' />
              {t('Recharge again')}
            </Button>
          ) : null}
          <Button variant='outline' onClick={() => props.onOpenChange(false)}>
            {t('Close')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
