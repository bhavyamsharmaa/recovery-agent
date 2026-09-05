import { useState } from 'react'
import BatchSummary from './components/BatchSummary'
import ControlPanel from './components/ControlPanel'
import EscalationQueue from './components/EscalationQueue'
import PaymentDetail from './components/PaymentDetail'
import PaymentsFeed from './components/PaymentsFeed'
import SectionNav from './components/SectionNav'

/**
 * Two views, switched by a piece of state.
 *
 * No router: this is a feed and a detail page, and react-router would add a
 * dependency, a provider, and a route table to express one nullable string. The
 * trade is real — there is no shareable URL for a payment, and the browser's
 * back button leaves the app rather than returning to the feed. If either of
 * those starts mattering, that is the moment to add the router, not before.
 */
export default function App() {
  const [selected, setSelected] = useState<string | null>(null)

  return (
    <div className="min-h-screen bg-slate-50">
      <header className="border-b border-slate-200 bg-white">
        <div className="mx-auto max-w-6xl px-4 py-4 sm:px-6">
          <h1 className="text-xl font-semibold text-slate-900">Recovery Agent Dashboard</h1>
          <p className="text-sm text-slate-500">
            Failed payments, and what the agent decided to do about each one.
          </p>
        </div>
      </header>

      {/* Only on the feed view. The detail view is one panel with its own back
          link, so jump links there would point at sections that are not on
          screen. */}
      {selected === null && <SectionNav />}

      <main className="mx-auto max-w-6xl px-4 py-8 sm:px-6">
        {selected === null ? (
          // The summary sits above the feed, not beside or below it: it is the
          // screen's headline answer, and the feed is the evidence behind it.
          //
          // Each section is wrapped in a div carrying the anchor id rather than
          // the id being added to the components themselves: the nav is a thing
          // laid over this layout, and a component should not have to know it
          // is being linked to. scroll-mt-14 offsets the sticky bar's height so
          // a jumped-to heading lands below it rather than beneath it.
          <div className="flex flex-col gap-8">
            <div id="batch-summary" className="scroll-mt-14">
              <BatchSummary />
            </div>
            <div id="control-panel" className="scroll-mt-14">
              <ControlPanel />
            </div>
            <div id="escalation-queue" className="scroll-mt-14">
              <EscalationQueue />
            </div>
            <div id="payments-feed" className="scroll-mt-14">
              <PaymentsFeed onSelect={setSelected} />
            </div>
          </div>
        ) : (
          <PaymentDetail paymentId={selected} onBack={() => setSelected(null)} />
        )}
      </main>
    </div>
  )
}
