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
import { History, Loader2 } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { SiWechat } from 'react-icons/si'

import { Button } from '@/components/ui/button'
import { TitledCard } from '@/components/ui/titled-card'

import { useWischoicerRecharge } from '../hooks/use-wischoicer-recharge'
import {
  WISCHOICER_RECHARGE_MAX_CENTS,
  WISCHOICER_RECHARGE_MIN_CENTS,
  WISCHOICER_RECHARGE_TIERS_CENTS,
  isWischoicerRechargeTerminal,
} from '../lib/wischoicer-recharge'
import { getWischoicerPhaseStatus } from '../lib/wischoicer-recharge-ui'
import { WischoicerRechargeHistoryDialog } from './dialogs/wischoicer-recharge-history-dialog'
import { WischoicerRechargeQrDialog } from './dialogs/wischoicer-recharge-qr-dialog'

interface WischoicerRechargeCardProps {
  onRechargeSuccess: () => void
}

export function WischoicerRechargeCard(props: WischoicerRechargeCardProps) {
  const { t } = useTranslation()
  const [qrOpen, setQrOpen] = useState(false)
  const [historyOpen, setHistoryOpen] = useState(false)

  const recharge = useWischoicerRecharge(props.onRechargeSuccess)

  // While availability is being probed, or once probed as unavailable (the
  // wallet façade is fail-closed until Token A + billing base URL are set), the
  // WeChat Native section stays hidden so legacy payment methods are unaffected.
  if (recharge.available !== true) {
    return null
  }

  const handleTier = async (amountCents: number) => {
    const ok = await recharge.createOrder(amountCents)
    if (ok) {
      setQrOpen(true)
    }
  }

  const hasActiveOrder =
    !!recharge.order && !isWischoicerRechargeTerminal(recharge.phase)
  const status = recharge.order
    ? getWischoicerPhaseStatus(recharge.phase, t)
    : null

  return (
    <>
      <TitledCard
        title={t('WeChat Recharge')}
        description={t('Recharge your balance via WeChat scan')}
        icon={<SiWechat className='h-5 w-5' style={{ color: '#07C160' }} />}
        action={
          <Button
            variant='outline'
            size='sm'
            onClick={() => setHistoryOpen(true)}
          >
            <History className='h-4 w-4' />
            <span className='hidden sm:inline'>{t('History')}</span>
          </Button>
        }
      >
        <div className='flex flex-col gap-4'>
          <div className='grid grid-cols-2 gap-2 sm:grid-cols-4'>
            {WISCHOICER_RECHARGE_TIERS_CENTS.map((cents) => (
              <Button
                key={cents}
                variant='outline'
                disabled={recharge.creating}
                onClick={() => handleTier(cents)}
                className='h-12 text-base font-semibold'
              >
                ¥{cents / 100}
              </Button>
            ))}
          </div>

          <p className='text-muted-foreground text-xs'>
            {t('Min ¥{{min}}, max ¥{{max}} per recharge', {
              min: WISCHOICER_RECHARGE_MIN_CENTS / 100,
              max: WISCHOICER_RECHARGE_MAX_CENTS / 100,
            })}
          </p>

          {recharge.creating ? (
            <div className='text-muted-foreground flex items-center gap-2 text-sm'>
              <Loader2 className='h-4 w-4 animate-spin' />
              {t('Creating order...')}
            </div>
          ) : null}

          {hasActiveOrder && status ? (
            <div className='flex items-center justify-between gap-2 rounded-lg border border-dashed p-3'>
              <div className='min-w-0'>
                <div className='text-foreground text-sm font-medium'>
                  {status.label}
                </div>
                {status.hint ? (
                  <div className='text-muted-foreground truncate text-xs'>
                    {status.hint}
                  </div>
                ) : null}
              </div>
              <Button size='sm' onClick={() => setQrOpen(true)}>
                {t('Continue')}
              </Button>
            </div>
          ) : null}

          {recharge.error ? (
            <p className='text-destructive text-xs'>{recharge.error}</p>
          ) : null}
        </div>
      </TitledCard>

      <WischoicerRechargeQrDialog
        open={qrOpen}
        onOpenChange={setQrOpen}
        order={recharge.order}
        phase={recharge.phase}
        remainingSeconds={recharge.remainingSeconds}
        expired={recharge.expired}
        onReset={recharge.reset}
      />

      <WischoicerRechargeHistoryDialog
        open={historyOpen}
        onOpenChange={setHistoryOpen}
      />
    </>
  )
}
