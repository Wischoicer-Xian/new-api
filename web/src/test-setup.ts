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
import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import i18next from 'i18next'
import { initReactI18next } from 'react-i18next'
import { afterEach, beforeAll } from 'vitest'

beforeAll(async () => {
  await i18next.use(initReactI18next).init({
    lng: 'en',
    fallbackLng: 'en',
    resources: {
      en: {
        translation: {},
      },
    },
  })
})

afterEach(() => {
  cleanup()
})

Object.defineProperty(window, 'matchMedia', {
  configurable: true,
  value: (query: string): MediaQueryList => ({
    matches: false,
    media: query,
    onchange: null,
    addListener: () => undefined,
    removeListener: () => undefined,
    addEventListener: () => undefined,
    removeEventListener: () => undefined,
    dispatchEvent: () => false,
  }),
})

window.requestAnimationFrame = (callback: FrameRequestCallback) =>
  window.setTimeout(() => callback(performance.now()), 0)
window.cancelAnimationFrame = (handle: number) => window.clearTimeout(handle)

class ResizeObserverMock {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

Object.defineProperty(globalThis, 'ResizeObserver', {
  configurable: true,
  value: ResizeObserverMock,
})

Object.defineProperty(HTMLElement.prototype, 'scrollIntoView', {
  configurable: true,
  value: () => undefined,
})

// jsdom may not expose `window.localStorage` in every Node/jsdom combination,
// while Node.js 25+ can expose global storage accessors that resolve to
// `undefined`. Keep one shared localStorage fallback for browser and global
// consumers, and provide the same in-memory behavior for sessionStorage.
function createMemoryStorage(): Storage {
  const entries = new Map<string, string>()
  return {
    get length() {
      return entries.size
    },
    clear: () => entries.clear(),
    getItem: (key) => entries.get(String(key)) ?? null,
    key: (index) => [...entries.keys()][index] ?? null,
    removeItem: (key) => {
      entries.delete(String(key))
    },
    setItem: (key, value) => {
      entries.set(String(key), String(value))
    },
  }
}

function getGlobalStorage(
  name: 'localStorage' | 'sessionStorage'
): Storage | undefined {
  try {
    const storage = globalThis[name]
    return typeof storage?.setItem === 'function' ? storage : undefined
  } catch {
    return undefined
  }
}

let localStorage: Storage | undefined
if (typeof window !== 'undefined') {
  try {
    localStorage = window.localStorage
  } catch {
    localStorage = undefined
  }

  if (!localStorage || typeof localStorage.setItem !== 'function') {
    localStorage = createMemoryStorage()
    Object.defineProperty(window, 'localStorage', {
      configurable: true,
      value: localStorage,
    })
  }
}

localStorage ??= getGlobalStorage('localStorage') ?? createMemoryStorage()

// Node's bare `localStorage` global can be separate from jsdom's window
// property. Expose the same store through both names so persisted stores behave
// consistently regardless of which global a module reads at import time.
Object.defineProperty(globalThis, 'localStorage', {
  configurable: true,
  enumerable: true,
  writable: true,
  value: localStorage,
})

if (
  typeof window !== 'undefined' &&
  getGlobalStorage('localStorage') !== window.localStorage
) {
  Object.defineProperty(window, 'localStorage', {
    configurable: true,
    value: localStorage,
  })
}

if (!getGlobalStorage('sessionStorage')) {
  Object.defineProperty(globalThis, 'sessionStorage', {
    configurable: true,
    enumerable: true,
    writable: true,
    value: createMemoryStorage(),
  })
}
