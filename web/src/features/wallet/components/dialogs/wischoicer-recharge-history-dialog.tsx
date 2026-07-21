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
import { Check, Copy, Loader2 } from 'lucide-react'
import { useCallback, useEffect, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Skeleton } from '@/components/ui/skeleton'
import { useCopyToClipboard } from '@/hooks/use-copy-to-clipboard'
import { formatTimestampToDate } from '@/lib/format'

import { listWischoicerRecharges } from '../../api'
import {
  formatCentsAsYuan,
  getWischoicerRechargePhase,
  toSafeRechargeList,
  type WischoicerWalletRechargeView,
} from '../../lib/wischoicer-recharge'
import { getWischoicerPhaseStatus } from '../../lib/wischoicer-recharge-ui'

const PAGE_SIZE = 20
const SKELETON_KEYS = ['skel-1', 'skel-2', 'skel-3', 'skel-4']

interface WischoicerRechargeHistoryDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
}

export function WischoicerRechargeHistoryDialog(
  props: WischoicerRechargeHistoryDialogProps
) {
  const { t } = useTranslation()
  const { copyToClipboard, copiedText } = useCopyToClipboard({ notify: false })

  const [items, setItems] = useState<WischoicerWalletRechargeView[]>([])
  const [nextCursor, setNextCursor] = useState<string | undefined>(undefined)
  const [loading, setLoading] = useState(false)
  const [loadingMore, setLoadingMore] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const loadFirst = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const res = await listWischoicerRecharges({ limit: PAGE_SIZE })
      if (res?.success && res.data) {
        // Project every item through the safe view before it hits state, so the
        // list path gets the same no-leak guarantee as create/get.
        setItems(toSafeRechargeList(res.data.items ?? []))
        setNextCursor(res.data.nextCursor)
      } else {
        setError(res?.message || t('Failed to load history'))
        setItems([])
        setNextCursor(undefined)
      }
    } catch {
      setError(t('Failed to load history'))
    } finally {
      setLoading(false)
    }
  }, [t])

  useEffect(() => {
    if (props.open) {
      void loadFirst()
    }
  }, [props.open, loadFirst])

  const loadMore = useCallback(async () => {
    if (!nextCursor || loadingMore) return
    setLoadingMore(true)
    try {
      const res = await listWischoicerRecharges({
        cursor: nextCursor,
        limit: PAGE_SIZE,
      })
      if (res?.success && res.data) {
        const data = res.data
        setItems((prev) => [...prev, ...toSafeRechargeList(data.items ?? [])])
        setNextCursor(data.nextCursor)
      }
    } catch {
      // keep existing items; a transient failure does not clear the list
    } finally {
      setLoadingMore(false)
    }
  }, [nextCursor, loadingMore])

  const renderContent = (): ReactNode => {
    if (loading) {
      return (
        <div className='space-y-2'>
          {SKELETON_KEYS.map((key) => (
            <div
              key={key}
              className='flex items-center justify-between rounded-lg border p-3'
            >
              <Skeleton className='h-4 w-24' />
              <Skeleton className='h-4 w-16' />
            </div>
          ))}
        </div>
      )
    }
    if (error) {
      return (
        <div className='text-muted-foreground py-8 text-center text-sm'>
          {error}
        </div>
      )
    }
    if (items.length === 0) {
      return (
        <div className='text-muted-foreground flex min-h-32 flex-col items-center justify-center py-8 text-center text-sm'>
          <p className='font-medium'>{t('No recharge records found')}</p>
          <p className='mt-1 text-xs'>
            {t('Your recharge history will appear here')}
          </p>
        </div>
      )
    }
    return (
      <div className='space-y-2'>
        {items.map((item) => {
          const status = getWischoicerPhaseStatus(
            getWischoicerRechargePhase(item.status),
            t
          )
          // Label the timestamp by what it actually is — never pass the QR
          // expiry off as a create/pay time. paidTime wins when the façade
          // exposes it (PR#19); otherwise show the order's validity window.
          let timeLabel: { label: string; value: string } | null = null
          if (item.paidTime) {
            timeLabel = {
              label: t('Payment time'),
              value: formatTimestampToDate(item.paidTime),
            }
          } else if (item.expireTime) {
            timeLabel = {
              label: t('Valid until'),
              value: formatTimestampToDate(item.expireTime),
            }
          }
          return (
            <div key={item.orderNo} className='rounded-lg border p-3'>
              <div className='flex items-start justify-between gap-2'>
                <div className='min-w-0'>
                  <div className='text-foreground text-sm font-semibold'>
                    ¥{formatCentsAsYuan(item.amountCents)}
                  </div>
                  <div className='text-muted-foreground mt-0.5 flex items-center gap-1 text-xs'>
                    <code className='truncate font-mono'>{item.orderNo}</code>
                    <Button
                      variant='ghost'
                      size='sm'
                      className='h-5 w-5 p-0'
                      onClick={() => copyToClipboard(item.orderNo)}
                    >
                      {copiedText === item.orderNo ? (
                        <Check className='h-3 w-3' />
                      ) : (
                        <Copy className='h-3 w-3' />
                      )}
                    </Button>
                  </div>
                </div>
                <StatusBadge
                  label={status.label}
                  variant={status.variant}
                  showDot
                  copyable={false}
                />
              </div>
              {timeLabel ? (
                <div className='text-muted-foreground mt-1 text-xs'>
                  {timeLabel.label}: {timeLabel.value}
                </div>
              ) : null}
            </div>
          )
        })}
      </div>
    )
  }

  return (
    <Dialog open={props.open} onOpenChange={props.onOpenChange}>
      <DialogContent className='sm:max-w-lg'>
        <DialogHeader>
          <DialogTitle>{t('Recharge History')}</DialogTitle>
          <DialogDescription>{t('Your WeChat recharge orders')}</DialogDescription>
        </DialogHeader>

        <div className='max-h-[60vh] overflow-y-auto pr-1'>{renderContent()}</div>

        {!loading && !error && nextCursor ? (
          <div className='flex justify-center'>
            <Button
              variant='outline'
              size='sm'
              onClick={loadMore}
              disabled={loadingMore}
            >
              {loadingMore ? (
                <Loader2 className='h-4 w-4 animate-spin' />
              ) : (
                t('Load more')
              )}
            </Button>
          </div>
        ) : null}
      </DialogContent>
    </Dialog>
  )
}
