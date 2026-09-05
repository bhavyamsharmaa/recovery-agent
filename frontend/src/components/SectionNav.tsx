/**
 * Jump links to the four sections of the dashboard.
 *
 * The page is four stacked panels and the payments feed at the bottom can be a
 * thousand rows, so reaching the control panel from the feed is a long scroll.
 * This is a navigation aid over the existing layout — it does not know what any
 * section contains and changes nothing about them.
 *
 * Anchors rather than buttons, deliberately: an <a href="#id"> is focusable,
 * keyboard-activatable and middle-clickable for free, and still works if the
 * JavaScript below never runs. The click handler exists only to add smooth
 * scrolling and to keep the URL clean; preventDefault is called after the
 * scroll is issued, so a browser without scrollIntoView still follows the href.
 */

/** The four sections, in the order they appear on the page. */
const SECTIONS = [
  { id: 'batch-summary', label: 'Batch summary' },
  { id: 'control-panel', label: 'Control panel' },
  { id: 'escalation-queue', label: 'Escalation queue' },
  { id: 'payments-feed', label: 'Payments feed' },
] as const

export default function SectionNav() {
  function jump(event: React.MouseEvent<HTMLAnchorElement>, id: string) {
    const target = document.getElementById(id)
    if (!target) return // fall through to the href

    // scroll-margin-top on the target (see App.tsx) keeps the heading clear of
    // this bar, so the section lands below it rather than under it.
    target.scrollIntoView({ behavior: 'smooth', block: 'start' })
    event.preventDefault()
  }

  return (
    <nav
      aria-label="Dashboard sections"
      className="sticky top-0 z-10 border-b border-slate-200 bg-white/95 backdrop-blur"
    >
      {/* flex-wrap rather than overflow-x-auto: four links must stay readable
          at 375px without sideways scrolling, and wrapping to two rows is the
          layout that does that. gap-x is tighter than gap-y so a wrapped pair
          still reads as one group. */}
      <div className="mx-auto flex max-w-6xl flex-wrap gap-x-4 gap-y-1 px-4 py-2 sm:gap-x-6 sm:px-6">
        {SECTIONS.map((s) => (
          <a
            key={s.id}
            href={`#${s.id}`}
            onClick={(e) => jump(e, s.id)}
            className="rounded text-xs font-medium text-slate-600 transition-colors hover:text-slate-900 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-slate-400 sm:text-sm"
          >
            {s.label}
          </a>
        ))}
      </div>
    </nav>
  )
}
