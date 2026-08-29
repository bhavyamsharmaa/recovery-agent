import { useState } from 'react'
import PaymentDetail from './components/PaymentDetail'
import PaymentsFeed from './components/PaymentsFeed'

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

      <main className="mx-auto max-w-6xl px-4 py-8 sm:px-6">
        {selected === null ? (
          <PaymentsFeed onSelect={setSelected} />
        ) : (
          <PaymentDetail paymentId={selected} onBack={() => setSelected(null)} />
        )}
      </main>
    </div>
  )
}
