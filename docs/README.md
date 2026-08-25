An agent that classifies failed payments and recovers them with policy-driven retries.

## Known limitations

- Attempt counters are in-memory through Day 4 and reset on process restart;
  persisted via Postgres from Day 5 onward.
- Webhook deduplication is likewise in-memory: after a restart, a redelivery of
  an event seen before the restart is processed as new.
