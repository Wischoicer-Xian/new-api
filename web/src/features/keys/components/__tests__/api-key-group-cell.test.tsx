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
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, test } from 'vitest'

const { createInstance } = await import('i18next')
const { I18nextProvider, initReactI18next } = await import('react-i18next')
const { TooltipProvider } = await import('@/components/ui/tooltip')
const { ApiKeyGroupCell } = await import('../api-key-group-cell')

const i18n = createInstance()
await i18n.use(initReactI18next).init({
  lng: 'en',
  resources: {
    en: {
      translation: {
        Auto: 'Auto',
        'Cross-group': 'Cross-group',
        Ratio: 'Ratio',
        'Automatically selects the best available group with circuit breaker mechanism':
          'Automatically selects the best available group with circuit breaker mechanism',
      },
    },
  },
})

function CellHarness(props: {
  group: string
  ratio?: number | string
  crossGroupRetry?: boolean
  shouldReduceMotion?: boolean
}) {
  return (
    <I18nextProvider i18n={i18n}>
      <TooltipProvider>
        <ApiKeyGroupCell
          group={props.group}
          ratio={props.ratio}
          crossGroupRetry={props.crossGroupRetry ?? false}
          shouldReduceMotion={props.shouldReduceMotion ?? false}
        />
      </TooltipProvider>
    </I18nextProvider>
  )
}

describe('API key group table cell', () => {
  test('renders two unclipped rings and a localized Auto ratio when API data uses a nonlocalized string', () => {
    const { container } = render(
      <CellHarness group='auto' ratio='自动' crossGroupRetry />
    )

    const badgeCell = container.querySelector<HTMLElement>(
      '[data-api-key-group-cell="auto"]'
    )
    expect(badgeCell).toHaveClass('overflow-visible')
    expect(badgeCell).not.toHaveClass('overflow-hidden')

    const frames = container.querySelectorAll('[data-auto-group-frame]')
    const movingRings = container.querySelectorAll(
      '[data-auto-group-flow-border]'
    )
    expect(frames.length).toBe(2)
    expect(movingRings.length).toBe(2)
    for (const frame of frames) {
      expect(frame).toHaveClass(
        'relative',
        'overflow-visible',
        'rounded-4xl',
        'p-px'
      )
    }

    const ratio = container.querySelector<HTMLElement>(
      '[data-auto-group-effect="ratio"]'
    )
    expect(ratio).toHaveTextContent('Auto Ratio')
    expect(ratio).not.toHaveTextContent('x')
    expect(container).not.toHaveTextContent('自动')
    expect(container).toHaveTextContent('Cross-group')

    const crossGroupBadge = [
      ...container.querySelectorAll<HTMLElement>('[data-slot="status-badge"]'),
    ].find((badge) => badge.textContent === 'Cross-group')
    expect(crossGroupBadge).not.toBeUndefined()
    expect(crossGroupBadge?.closest('[data-auto-group-frame]')).toBeNull()
  })

  test('keeps both static Auto frames but omits their moving layers for reduced motion', () => {
    const { container } = render(
      <CellHarness group='auto' ratio='Auto' shouldReduceMotion />
    )

    expect(container.querySelectorAll('[data-auto-group-frame]').length).toBe(2)
    expect(
      container.querySelectorAll('[data-auto-group-flow-border]').length
    ).toBe(0)
  })

  test('shows the Auto badge when ratio data is unavailable', () => {
    const { container } = render(
      <CellHarness group='auto' shouldReduceMotion={false} />
    )

    expect(container.querySelectorAll('[data-auto-group-frame]').length).toBe(1)
    expect(
      container.querySelectorAll('[data-auto-group-flow-border]').length
    ).toBe(1)
    expect(container.querySelector('[data-auto-group-effect="ratio"]')).toBe(
      null
    )
    expect(container).toHaveTextContent('Cross-group')
    expect(container).toHaveTextContent('Auto')
    expect(container).not.toHaveTextContent('Ratio')
  })

  test.each([
    [0.8, 'bg-info/10', 'text-info', 'border-info/30'],
    [1, 'bg-muted', 'text-muted-foreground', 'border-muted-foreground/30'],
    [3, 'bg-warning/10', 'text-warning', 'border-warning/30'],
  ])(
    'preserves the original %s multiplier color in the compact layout',
    (ratio, background, color, border) => {
      const { container } = render(
        <CellHarness group='default' ratio={ratio} />
      )
      const multiplier = screen.getByText(`${ratio}x`).parentElement
      expect(multiplier).toHaveClass(
        background,
        color,
        border,
        'rounded-full',
        'tabular-nums',
        'h-5',
        'min-w-12'
      )
      expect(
        container.querySelector('[data-auto-group-flow-border]')
      ).toBeNull()
    }
  )

  test('labels the user group multiplier as inherited without inventing a numeric value', async () => {
    render(<CellHarness group='' />)
    expect(screen.getByText('User Group')).toBeInTheDocument()
    expect(screen.getByText('Inherited')).toBeInTheDocument()
    expect(screen.getByText('Inherited').parentElement).toHaveClass(
      'border-muted-foreground/30',
      'rounded-full'
    )
    expect(screen.queryByText('1x')).not.toBeInTheDocument()
    await userEvent.tab()
    expect(await screen.findByText('Follow user group')).toBeVisible()
  })

  test('keeps a long group name and exact multiplier available through keyboard focus', async () => {
    const groupName = 'production-with-a-very-long-custom-group-name'
    render(<CellHarness group={groupName} ratio={12.345678} />)
    expect(
      screen.getByText(groupName).closest('[data-slot="tooltip-trigger"]')
    ).toHaveClass('max-w-50')
    expect(screen.getByText('12.345678x')).toBeInTheDocument()
    await userEvent.tab()
    expect(
      await screen.findByText(groupName, {
        selector: '[data-slot="tooltip-content"]',
      })
    ).toBeVisible()
  })

  test('never turns a string-valued normal group ratio into an automatic multiplier', () => {
    render(<CellHarness group='vip' ratio='自动' />)
    expect(screen.getByText('vip')).toBeInTheDocument()
    expect(screen.queryByText('Auto')).not.toBeInTheDocument()
    expect(screen.queryByText('自动')).not.toBeInTheDocument()
  })
})
