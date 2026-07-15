import path from 'node:path'

import { defineConfig } from 'vitest/config'

// vitest config for web/default. Mirrors the rsbuild `@/` -> ./src alias so
// component tests resolve the same imports as the build. The jsdom environment
// lets @testing-library/react render the DOM; the setup file wires jest-dom
// matchers.
export default defineConfig({
  resolve: {
    alias: {
      '@': path.resolve(import.meta.dirname, 'src'),
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./vitest.setup.ts'],
    include: ['src/**/*.{test,spec}.{ts,tsx}'],
    // Pre-existing Node/Bun test-runner files (node:test, bun:test) live in the
    // tree without a vitest runner; they are not vitest suites, so exclude them
    // rather than report false failures. They remain runnable via their own
    // runners (`node --test`, `bun test`).
    exclude: [
      '**/node_modules/**',
      '**/dist/**',
      'src/features/dashboard/lib/flow.test.ts',
      'src/features/dashboard/lib/flow-selection.test.ts',
      'src/components/ui/dropdown-menu.test.tsx',
      'src/features/wallet/hooks/use-wischoicer-recharge.test.ts',
      'src/features/wallet/lib/wischoicer-recharge.test.ts',
    ],
    css: false,
  },
})
