import '@testing-library/jest-dom/vitest'

// jsdom does not implement matchMedia; base-ui components (e.g. Select content)
// query it during render. Provide a stub so rendering does not crash tests.
if (typeof window !== 'undefined' && typeof window.matchMedia !== 'function') {
  Object.defineProperty(window, 'matchMedia', {
    value: (query: string) => ({
      matches: false,
      media: query,
      onchange: null,
      addListener: () => {},
      removeListener: () => {},
      addEventListener: () => {},
      removeEventListener: () => {},
      dispatchEvent: () => false,
    }),
    configurable: true,
  })
}

// jsdom may not expose `window.localStorage` in every Node/jsdom combination,
// but several stores (e.g. zustand auth-store) touch it at module-load time.
// Provide an in-memory stub so importing those modules during a test does not
// crash before any test logic runs.
if (typeof window !== 'undefined' && !window.localStorage) {
  const store = new Map<string, string>()
  const stub: Storage = {
    getItem: (key: string) => store.get(key) ?? null,
    setItem: (key: string, value: string) => {
      store.set(key, String(value))
    },
    removeItem: (key: string) => {
      store.delete(key)
    },
    clear: () => {
      store.clear()
    },
    key: (index: number) => [...store.keys()][index] ?? null,
    get length() {
      return store.size
    },
  }
  Object.defineProperty(window, 'localStorage', {
    value: stub,
    configurable: true,
  })
}
