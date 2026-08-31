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

    // One worker for the whole run, not one per file.
    //
    // fileParallelism: false alone was not enough. The pool gives each worker
    // 60s to hand-shake, jsdom's environment setup costs ~19s here, and the
    // second file's worker still exceeded the budget: BatchSummary.test.tsx
    // never started, while the run reported "Test Files 1 passed (1)" with the
    // timeout relegated to an "Unhandled Error" block above the summary. A file
    // that does not run must not be able to look like a file that passed.
    //
    // The timeout is hit during worker startup, so the fix is to start fewer
    // workers rather than to wait longer for each.
    // maxWorkers is a top-level option in Vitest 4; the v3 spelling
    // poolOptions.threads.singleThread was removed in that release and is
    // ignored without erroring, which would leave this fix resting on nothing.
    // minWorkers is not in the config type and fails `tsc -b`, so only the
    // ceiling is set — it is the constraint that matters.
    maxWorkers: 1,

    // A run that loses a file must not exit 0.
    //
    // maxWorkers: 1 made the startup timeout rare rather than impossible — the
    // run still occasionally reports "Test Files 1 passed (1)" with the jsdom
    // file missing, depending on what else the machine is doing. Tuning a race
    // is not a fix, so the race is stopped from passing silently instead:
    // dangerouslyIgnoreUnhandledErrors defaults to false, and these two make
    // the consequence explicit rather than advisory.
    //
    // passWithNoTests catches only the total-wipeout case. The one-file-missing
    // case is caught by scripts/check-suite.mjs, which `npm test` runs after
    // vitest and which fails unless every file in `include` reported.
    passWithNoTests: false,
    dangerouslyIgnoreUnhandledErrors: false,
  },
})
