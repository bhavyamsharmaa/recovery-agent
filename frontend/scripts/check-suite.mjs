// Fails the test run unless every test file actually reported a result.
//
// Vitest's own exit code does not cover this. When a worker exceeds the pool's
// startup timeout, the file it was going to run is dropped, the timeout is
// printed as a non-fatal "Unhandled Error" block, and the summary reports the
// files that did run as passing — "Test Files 1 passed (1)" with a green tally
// and exit code 0. A file that never ran is indistinguishable from a file with
// no failures unless something counts them.
//
// This reads vitest's JSON report and compares the set of files that reported
// against the set of files on disk matching the same include glob. Comparing
// against the glob rather than a hardcoded number means adding a test file
// needs no change here, and deleting one does not silently lower the bar.
import { glob, readFile } from 'node:fs/promises'

const REPORT = 'node_modules/.vitest-report.json'
const INCLUDE = 'src/**/*.test.{ts,tsx}'

// Paths are normalised to forward slashes throughout: the glob returns Windows
// separators here while vitest's report carries POSIX ones, and comparing the
// two raw would report every file as missing on this machine.
const slash = (p) => p.replace(/\\/g, '/')

const expected = new Set()
for await (const f of glob(INCLUDE)) expected.add(slash(f))

if (expected.size === 0) {
  console.error(`check-suite: no files matched ${INCLUDE}`)
  process.exit(1)
}

let report
try {
  report = JSON.parse(await readFile(REPORT, 'utf8'))
} catch (err) {
  console.error(`check-suite: could not read ${REPORT}: ${err.message}`)
  process.exit(1)
}

// The report gives absolute paths; reduce them to the same repo-relative shape
// the glob produces by cutting at the last occurrence of the include root.
const reported = new Set(
  (report.testResults ?? []).map((r) => {
    const p = slash(r.name)
    const at = p.lastIndexOf('/src/')
    return at === -1 ? p : p.slice(at + 1)
  }),
)

const missing = [...expected].filter((f) => !reported.has(f))

if (missing.length > 0) {
  console.error(
    `\ncheck-suite: ${missing.length} test file(s) did not run:\n` +
      missing.map((f) => `  - ${f}`).join('\n') +
      `\n\nThese files exist and match ${INCLUDE} but reported no result.\n` +
      `The usual cause on this machine is a worker exceeding the pool's\n` +
      `startup timeout, which vitest reports as a non-fatal error beside a\n` +
      `green summary. The tests in these files did not pass; they did not run.\n`,
  )
  process.exit(1)
}

console.log(`check-suite: all ${expected.size} test files reported`)
