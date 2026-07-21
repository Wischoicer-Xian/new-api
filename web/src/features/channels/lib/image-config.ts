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
export type ImageOperation = 'generation' | 'edit'
export type ImageMode = 'sync' | 'async_task'
export type ImageOverride = {
  model: string
  operation: ImageOperation
  mode: ImageMode
}

export const IMAGE_OPERATIONS: ImageOperation[] = ['generation', 'edit']

export function isImageMode(value: unknown): value is ImageMode {
  return value === 'sync' || value === 'async_task'
}

// parseImageConfig derives a structured view from the persisted
// image_execution_config JSON string. Unknown or malformed values are dropped
// rather than carried forward, so the form field is the single source of truth
// and the editor never silently preserves an invalid shape.
export function parseImageConfig(raw: string | undefined | null): {
  generation: string
  edit: string
  overrides: ImageOverride[]
} {
  const base = { generation: '', edit: '', overrides: [] as ImageOverride[] }
  if (!raw || !raw.trim()) return base
  try {
    const obj = JSON.parse(raw) as {
      defaults?: Record<string, unknown>
      models?: Record<string, Record<string, unknown>>
    }
    const generation = obj.defaults?.generation
    const edit = obj.defaults?.edit
    const overrides: ImageOverride[] = []
    if (obj.models) {
      for (const [model, ops] of Object.entries(obj.models)) {
        if (!ops || typeof ops !== 'object') continue
        for (const [op, mode] of Object.entries(ops)) {
          if ((op === 'generation' || op === 'edit') && isImageMode(mode)) {
            overrides.push({
              model,
              operation: op,
              mode: mode as ImageMode,
            })
          }
        }
      }
    }
    return {
      generation: isImageMode(generation) ? (generation as ImageMode) : '',
      edit: isImageMode(edit) ? (edit as ImageMode) : '',
      overrides,
    }
  } catch {
    return base
  }
}

// serializeImageConfig rebuilds the image_execution_config JSON from the
// structured editor state. Returns an empty string when nothing is configured
// so an untouched channel stays out of the image task candidate pool (the
// cleanup path: a cleared editor serializes to '').
export function serializeImageConfig(
  generation: string,
  edit: string,
  overrides: ImageOverride[]
): string {
  const defaults: Record<string, ImageMode> = {}
  if (isImageMode(generation)) defaults.generation = generation
  if (isImageMode(edit)) defaults.edit = edit
  const models: Record<string, Record<string, ImageMode>> = {}
  for (const override of overrides) {
    const model = override.model.trim()
    if (!model) continue
    if (!models[model]) models[model] = {}
    models[model][override.operation] = override.mode
  }
  const obj: Record<string, unknown> = {}
  if (Object.keys(defaults).length > 0) obj.defaults = defaults
  if (Object.keys(models).length > 0) obj.models = models
  return Object.keys(obj).length > 0 ? JSON.stringify(obj) : ''
}

// shouldClearImageConfig decides whether a stale image_execution_config must be
// wiped on a channel-type switch. It clears ONLY when the capability preview
// SUCCEEDED and explicitly reported the type as not image-capable
// (image_capable === false). A preview business error ({success:false}, e.g. an
// unknown channel type or a malformed config) must NOT clear the field — the
// input is preserved so the admin can fix it and the error is surfaced.
export function shouldClearImageConfig(
  previewSuccess: boolean,
  imageCapable: boolean | undefined,
  configRaw: string
): boolean {
  return (
    previewSuccess === true && imageCapable === false && configRaw.trim() !== ''
  )
}

// canAddOverride decides whether the "Add override" action is permitted. The
// model must be non-empty AND the chosen mode must belong to the current
// operation's support set, so an unsupported mode can never be written even
// before the convergence effect resets the picker.
export function canAddOverride(
  model: string,
  mode: ImageMode,
  allowedModes: ImageMode[]
): boolean {
  return model.trim() !== '' && allowedModes.includes(mode)
}
