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
import { useQuery } from '@tanstack/react-query'
import { ImageIcon, Plus, Trash2 } from 'lucide-react'
import { useEffect, useState } from 'react'
import type { UseFormReturn } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import {
  SideDrawerSection,
  SideDrawerSectionHeader,
} from '@/components/drawer-layout'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

import { previewImageCapability } from '../../../api'
import type { ChannelFormValues } from '../../../lib/channel-form'
import {
  canAddOverride,
  IMAGE_OPERATIONS,
  isImageMode,
  parseImageConfig,
  serializeImageConfig,
  shouldClearImageConfig,
  type ImageMode,
  type ImageOperation,
  type ImageOverride,
} from '../../../lib/image-config'

type ChannelImageCapabilitySectionProps = {
  form: UseFormReturn<ChannelFormValues>
}

export function ChannelImageCapabilitySection({
  form,
}: ChannelImageCapabilitySectionProps) {
  const { t } = useTranslation()
  const type = form.watch('type')
  const configRaw = form.watch('image_execution_config') ?? ''

  const previewQuery = useQuery({
    queryKey: ['channel-image-capability-preview', type, configRaw],
    queryFn: () => previewImageCapability(type, configRaw),
    enabled: typeof type === 'number',
  })

  const data = previewQuery.data?.data
  const previewSuccess = previewQuery.data?.success === true
  const previewErrorMessage =
    previewQuery.data && previewQuery.data.success === false
      ? previewQuery.data.message
      : undefined
  const support = data?.support ?? {}
  const view = parseImageConfig(configRaw)

  const [newModel, setNewModel] = useState('')
  const [newOperation, setNewOperation] = useState<ImageOperation>('generation')
  const [newMode, setNewMode] = useState<ImageMode>('sync')

  // P1-4 A: only clear a stale image_execution_config when the preview SUCCEEDED
  // and explicitly reported image_capable === false (a registered adapter that
  // the current type maps to). A preview business error ({success:false}, e.g.
  // an unknown type or malformed config) must NOT silently wipe the field — the
  // input is preserved and the error surfaced below so the admin can fix it.
  useEffect(() => {
    if (
      shouldClearImageConfig(previewSuccess, data?.image_capable, configRaw)
    ) {
      form.setValue('image_execution_config', '', {
        shouldDirty: true,
        shouldValidate: true,
      })
    }
  }, [previewSuccess, data, configRaw, form])

  // Converge the override mode picker when the chosen operation or channel
  // support set changes so it never holds a mode the adapter does not support.
  // Depends on the react-query `data` (referentially stable between renders)
  // rather than the derived `support` map, which is recomputed every render.
  useEffect(() => {
    if (!data) {
      return
    }
    const allowed = (data.support?.[newOperation] ?? []).filter(
      isImageMode
    ) as ImageMode[]
    if (allowed.length > 0 && !allowed.includes(newMode)) {
      setNewMode(allowed[0])
    }
  }, [newOperation, data, newMode])

  // Loading / not yet resolved: render nothing.
  if (!previewQuery.data) {
    return null
  }
  // P1-4 A: a preview business error (HTTP 200 {success:false}, e.g. unknown
  // channel type or malformed config) must preserve the field and surface the
  // error rather than silently clearing it.
  if (!previewSuccess) {
    return (
      <SideDrawerSection>
        <SideDrawerSectionHeader
          title={t('Image Task Execution')}
          description={t(
            'How single-image tasks run on this channel. Unsupported modes are disabled.'
          )}
          icon={<ImageIcon className='h-4 w-4' aria-hidden='true' />}
        />
        <p className='text-xs text-red-500'>
          {previewErrorMessage ?? t('Failed to resolve image capability')}
        </p>
      </SideDrawerSection>
    )
  }
  // Preview succeeded but the channel type is not image-capable: the controls
  // are irrelevant. The cleanup effect has already cleared any stale config.
  if (!data || !data.image_capable) {
    return null
  }

  const writeConfig = (
    generation: string,
    edit: string,
    overrides: ImageOverride[]
  ) => {
    form.setValue(
      'image_execution_config',
      serializeImageConfig(generation, edit, overrides),
      {
        shouldDirty: true,
        shouldValidate: true,
      }
    )
  }

  const supportFor = (op: ImageOperation): ImageMode[] => {
    const list = support[op] ?? []
    return list.filter(isImageMode) as ImageMode[]
  }

  const addOverride = () => {
    const model = newModel.trim()
    if (!model) return
    // Dedup by model + operation: a model cannot carry two modes for the same
    // operation, so keep the existing entry rather than appending a duplicate.
    const exists = view.overrides.some(
      (o) => o.model === model && o.operation === newOperation
    )
    if (exists) {
      setNewModel('')
      return
    }
    writeConfig(view.generation, view.edit, [
      ...view.overrides,
      { model, operation: newOperation, mode: newMode },
    ])
    setNewModel('')
  }

  const removeOverride = (index: number) => {
    writeConfig(
      view.generation,
      view.edit,
      view.overrides.filter((_, i) => i !== index)
    )
  }

  return (
    <SideDrawerSection>
      <SideDrawerSectionHeader
        title={t('Image Task Execution')}
        description={t(
          'How single-image tasks run on this channel. Unsupported modes are disabled.'
        )}
        icon={<ImageIcon className='h-4 w-4' aria-hidden='true' />}
      />

      <div className='flex flex-col gap-4'>
        {IMAGE_OPERATIONS.map((op) => {
          const current = op === 'generation' ? view.generation : view.edit
          const options = supportFor(op)
          return (
            <div key={op} className='flex flex-col gap-1.5'>
              <label className='text-muted-foreground text-xs capitalize'>
                {t(op)}
              </label>
              <Select
                value={current}
                onValueChange={(value) => {
                  const next = value ?? ''
                  writeConfig(
                    op === 'generation' ? next : view.generation,
                    op === 'edit' ? next : view.edit,
                    view.overrides
                  )
                }}
              >
                <SelectTrigger>
                  <SelectValue
                    placeholder={
                      options.length > 0
                        ? t('Adapter default')
                        : t('Not supported')
                    }
                  />
                </SelectTrigger>
                <SelectContent>
                  {options.map((mode) => (
                    <SelectItem key={mode} value={mode}>
                      {mode}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
              {current && !options.includes(current as ImageMode) ? (
                <p className='text-xs text-red-500'>
                  {t('Configured mode is not supported by this adapter')}
                </p>
              ) : null}
            </div>
          )
        })}

        <div className='flex flex-col gap-2'>
          <label className='text-muted-foreground text-xs'>
            {t('Model overrides')}
          </label>
          {view.overrides.length === 0 ? (
            <p className='text-muted-foreground text-xs'>
              {t('No per-model overrides. Add one below.')}
            </p>
          ) : (
            <ul className='flex flex-col gap-2'>
              {view.overrides.map((override, index) => (
                <li
                  key={`${override.model}-${override.operation}`}
                  className='text-muted-foreground flex items-center justify-between gap-2 rounded-md border px-2 py-1.5 text-xs'
                >
                  <span className='truncate'>
                    {override.model} · {override.operation} · {override.mode}
                  </span>
                  <Button
                    type='button'
                    variant='ghost'
                    size='icon'
                    className='h-6 w-6'
                    onClick={() => removeOverride(index)}
                    aria-label={t('Remove override')}
                  >
                    <Trash2 className='h-3.5 w-3.5' aria-hidden='true' />
                  </Button>
                </li>
              ))}
            </ul>
          )}

          <div className='flex flex-wrap items-center gap-2'>
            <Input
              className='h-8 min-w-[140px] flex-1'
              placeholder={t('Model name')}
              value={newModel}
              onChange={(e) => setNewModel(e.target.value)}
            />
            <Select
              value={newOperation}
              onValueChange={(value) =>
                setNewOperation((value ?? 'generation') as ImageOperation)
              }
            >
              <SelectTrigger className='h-8 w-[130px]'>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {IMAGE_OPERATIONS.map((op) => (
                  <SelectItem key={op} value={op}>
                    {op}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Select
              value={newMode}
              onValueChange={(value) =>
                setNewMode((value ?? 'sync') as ImageMode)
              }
            >
              <SelectTrigger className='h-8 w-[110px]'>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {supportFor(newOperation).map((mode) => (
                  <SelectItem key={mode} value={mode}>
                    {mode}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Button
              type='button'
              variant='outline'
              size='sm'
              onClick={addOverride}
              disabled={
                !canAddOverride(newModel, newMode, supportFor(newOperation))
              }
            >
              <Plus className='h-3.5 w-3.5' aria-hidden='true' />
              {t('Add')}
            </Button>
          </div>
        </div>

        {data.preview && data.preview.length > 0 ? (
          <div className='flex flex-col gap-1'>
            <span className='text-muted-foreground text-xs'>
              {t('Resolved')}
            </span>
            <ul className='flex flex-col gap-1'>
              {data.preview.map((entry) => (
                <li
                  key={`${entry.operation}-${entry.model ?? ''}`}
                  className='text-xs'
                >
                  <span className='capitalize'>{entry.operation}</span>
                  {entry.model ? <span> · {entry.model}</span> : null}
                  <span>: {entry.ok ? entry.mode : t('fail-closed')}</span>
                  {entry.source ? (
                    <span className='text-muted-foreground'>
                      {' '}
                      ({entry.source})
                    </span>
                  ) : null}
                </li>
              ))}
            </ul>
          </div>
        ) : null}
      </div>
    </SideDrawerSection>
  )
}
