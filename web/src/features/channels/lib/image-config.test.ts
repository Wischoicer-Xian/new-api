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
import { describe, expect, it } from 'vitest'

import {
  canAddOverride,
  isImageMode,
  parseImageConfig,
  serializeImageConfig,
  shouldClearImageConfig,
} from './image-config'

describe('parseImageConfig / serializeImageConfig', () => {
  it('round-trips defaults and overrides', () => {
    const raw = serializeImageConfig('sync', 'sync', [
      { model: 'gpt-image-1', operation: 'edit', mode: 'sync' },
    ])
    const view = parseImageConfig(raw)
    expect(view.generation).toBe('sync')
    expect(view.edit).toBe('sync')
    expect(view.overrides).toEqual([
      { model: 'gpt-image-1', operation: 'edit', mode: 'sync' },
    ])
  })

  it('serializes an empty editor to an empty string (cleanup path)', () => {
    expect(serializeImageConfig('', '', [])).toBe('')
  })

  it('drops unknown operations and modes rather than carrying them', () => {
    const view = parseImageConfig(
      '{"defaults":{"generation":"quantum"},"models":{"m":{"upscale":"sync"}}}'
    )
    expect(view.generation).toBe('')
    expect(view.overrides).toEqual([])
  })

  it('treats malformed JSON as an empty view', () => {
    expect(parseImageConfig('{not json')).toEqual({
      generation: '',
      edit: '',
      overrides: [],
    })
  })
})

describe('shouldClearImageConfig (P1-4 A: fail-closed cleanup)', () => {
  const config = '{"defaults":{"generation":"sync"}}'

  it('clears when preview succeeded and the type is not image-capable', () => {
    expect(shouldClearImageConfig(true, false, config)).toBe(true)
  })

  it('preserves config on a preview business error (success:false)', () => {
    expect(shouldClearImageConfig(false, undefined, config)).toBe(false)
  })

  it('preserves config when the type is image-capable', () => {
    expect(shouldClearImageConfig(true, true, config)).toBe(false)
  })

  it('does not clear an already-empty config', () => {
    expect(shouldClearImageConfig(true, false, '')).toBe(false)
  })
})

describe('canAddOverride (P1-4 B: support-set gating)', () => {
  it('allows when model is set and mode is in the support set', () => {
    expect(canAddOverride('gpt-image-1', 'sync', ['sync'])).toBe(true)
  })

  it('denies when the mode is not in the support set', () => {
    expect(canAddOverride('gpt-image-1', 'async_task', ['sync'])).toBe(false)
  })

  it('denies when the model is empty', () => {
    expect(canAddOverride('   ', 'sync', ['sync'])).toBe(false)
  })

  it('denies when the support set is empty', () => {
    expect(canAddOverride('m', 'sync', [])).toBe(false)
  })
})

describe('isImageMode', () => {
  it('accepts sync and async_task, rejects everything else', () => {
    expect(isImageMode('sync')).toBe(true)
    expect(isImageMode('async_task')).toBe(true)
    expect(isImageMode('quantum')).toBe(false)
    expect(isImageMode(undefined)).toBe(false)
  })
})
