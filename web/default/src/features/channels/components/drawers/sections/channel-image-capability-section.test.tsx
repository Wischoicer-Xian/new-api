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
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import '@testing-library/jest-dom/vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import { useState } from 'react'
import { useForm, type UseFormReturn } from 'react-hook-form'
import { afterEach, describe, expect, it, vi, beforeEach } from 'vitest'

import { previewImageCapability } from '../../../api'
import type { ChannelFormValues } from '../../../lib/channel-form'
import { ChannelImageCapabilitySection } from './channel-image-capability-section'

vi.mock('react-i18next', () => ({
  useTranslation: () => ({ t: (key: string) => key }),
}))

vi.mock('../../../api', () => ({
  previewImageCapability: vi.fn(),
}))

type PreviewResult = Awaited<ReturnType<typeof previewImageCapability>>

type FormHolder = { current: UseFormReturn<ChannelFormValues> | null }

// Harness is a real component so useForm runs inside a React render (hooks
// cannot be called from the plain renderSection helper). It publishes the form
// instance via a holder so tests can assert field values.
function Harness({
  type,
  config,
  formHolder,
}: {
  type: number
  config: string
  formHolder: FormHolder
}) {
  const form = useForm<ChannelFormValues>({
    defaultValues: {
      type,
      image_execution_config: config,
    } as Partial<ChannelFormValues> as ChannelFormValues,
  })
  formHolder.current = form
  const [queryClient] = useState(
    () =>
      new QueryClient({
        defaultOptions: { queries: { retry: false, gcTime: 0 } },
      })
  )
  return (
    <QueryClientProvider client={queryClient}>
      <ChannelImageCapabilitySection form={form} />
    </QueryClientProvider>
  )
}

function renderSection(options: {
  type: number
  config: string
  preview: PreviewResult
}) {
  // Set the mock BEFORE rendering so the first query resolves to this value.
  vi.mocked(previewImageCapability).mockResolvedValue(options.preview)
  const formHolder: FormHolder = { current: null }
  const utils = render(
    <Harness
      type={options.type}
      config={options.config}
      formHolder={formHolder}
    />
  )
  return { ...utils, formInstance: formHolder.current }
}

describe('ChannelImageCapabilitySection (P1-4 fail-closed paths)', () => {
  beforeEach(() => {
    vi.resetAllMocks()
  })

  afterEach(() => {
    cleanup()
  })

  it('preserves config and surfaces the message on a preview business error', async () => {
    const config = '{"defaults":{"generation":"sync"}}'
    const { formInstance: form } = renderSection({
      type: 99999,
      config,
      preview: { success: false, message: '未知渠道类型 99999' },
    })

    // Error text is rendered (not silently swallowed).
    expect(await screen.findByText('未知渠道类型 99999')).toBeInTheDocument()
    // The config is preserved so the admin can fix it, not wiped.
    if (!form) {
      throw new Error('form instance was not captured from the harness')
    }
    expect(form.getValues('image_execution_config')).toBe(config)
  })

  it('clears stale config only when preview succeeds with image_capable false', async () => {
    const { formInstance: form } = renderSection({
      type: 14, // Anthropic: registered API type but not image-capable
      config: '{"defaults":{"generation":"sync"}}',
      preview: {
        success: true,
        data: { image_capable: false, support: {}, preview: [] },
      },
    })
    expect(form).toBeTruthy()
    if (!form) {
      throw new Error('form instance was not captured from the harness')
    }
    const setValueSpy = vi.spyOn(form, 'setValue')

    // The cleanup effect must fire setValue('image_execution_config', '') once
    // the preview resolves with success && image_capable === false. Asserting
    // the call (rather than rhf's getValue, which lags inside the test harness)
    // proves the fail-closed clear path fired.
    await waitFor(() => {
      expect(setValueSpy).toHaveBeenCalledWith(
        'image_execution_config',
        '',
        expect.objectContaining({ shouldDirty: true })
      )
    })
  })

  it('renders the editor when the channel type is image-capable', async () => {
    renderSection({
      type: 1,
      config: '',
      preview: {
        success: true,
        data: {
          image_capable: true,
          support: { generation: ['sync'], edit: ['sync'] },
          preview: [],
        },
      },
    })

    expect(await screen.findByText('Image Task Execution')).toBeInTheDocument()
  })

  it('disables Add until a model is entered (support-set gating wired)', async () => {
    const { container } = renderSection({
      type: 1,
      config: '',
      preview: {
        success: true,
        data: {
          image_capable: true,
          support: { generation: ['sync'], edit: ['sync'] },
          preview: [],
        },
      },
    })

    await screen.findByText('Image Task Execution')
    // The Add button exists and is disabled while no model is entered.
    const addButton = [...container.querySelectorAll('button')].find((b) =>
      b.textContent?.includes('Add')
    )
    expect(addButton).toBeDefined()
    expect(addButton).toBeDisabled()
  })
})
