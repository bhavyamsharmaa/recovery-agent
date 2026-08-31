import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
// defineConfig comes from vitest/config, not vite: vite's own type does not
// know about the `test` key, and using it makes `npm run build` fail type
// checking even though the config is valid. vitest re-exports vite's config
// type widened with its own options.
import { defineConfig } from 'vitest/config'

// Tailwind v4 is wired in as a Vite plugin rather than through PostCSS and a
// tailwind.config.js. That is the current standard integration: v4 discovers
// its own content sources and is configured from CSS, so there is no config
// file to keep in step with the source layout.
// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss()],

  // Vitest, scoped to the recovery-math surface rather than the whole app. The
  // jsdom environment is needed only by the one test that renders the summary
  // to prove a negative improvement still displays both figures; the arithmetic
  // tests beside it are plain functions and need no DOM at all.
  test: {
    environment: 'jsdom',
    include: ['src/**/*.test.{ts,tsx}'],

    // Threads rather than the default forks pool, and one file at a time.
    //
    // On this Windows machine worker startup is slow enough that two workers
    // racing to hand-shake in parallel both hit the pool's 60s timeout. The run
    // then dies with "Timeout waiting for worker to respond" and reports
    // "no tests" — which reads like a pass to anyone skimming the output, and is
    // the worst possible failure mode for a test suite.
    //
    // Serial threads start reliably and the whole suite runs in about 23s. The
    // cost is no parallelism across files, which is irrelevant at two files.
    pool: 'threads',
    fileParallelism: false,
  },
})
