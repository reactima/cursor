// vitest.config.ts
import { defineConfig } from 'vitest/config'

export default defineConfig({
  test: {
    globals: true, // Enables global variables like `describe`, `test`, etc.
    environment: 'jsdom', // Use jsdom for testing DOM components
    setupFiles: './vitest.globalsetup.ts', // Path to a setup file for any global setup
    coverage: {
      provider: 'c8', // Using 'c8' as the coverage provider
      reporter: ['text', 'json', 'html'], // Types of coverage reports
      reportsDirectory: './coverage', // Directory to output coverage reports
    },
  },
})
