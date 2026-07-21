import path from 'node:path'

import { defineConfig } from 'vitest/config'

// Vitest config for the DOM-heavy component tests. Other tests use node:test
// and are executed by Bun, which can run both node:test and Vitest-style APIs.
// Mirrors the rsbuild `@/` -> ./src alias so
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
    include: [
      'src/features/channels/components/drawers/sections/channel-image-capability-section.test.tsx',
      'src/features/channels/lib/image-config.test.ts',
    ],
    css: false,
  },
})
