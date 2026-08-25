# Failure Taxonomy & Retry Budgets

## Retry budgets

| Category | Retry budget | Spacing | Reasoning |
|---|---|---|---|
| `insufficient_funds` | 1 | immediate | One retry catches a false decline; beyond that, balance hasn't changed. Prefer `suggest_alternate_method` after. |
| `bank_downtime` | 3 | 30 min apart | External, bank-side outage — often short-lived but retrying immediately just fails identically. |
| `hard_decline` | 0 | n/a | Fraud flag, blocked/expired card, risk check — retrying the identical card fails identically every time. Escalate immediately. |
| `soft_decline` | 2 | immediate | Customer-fixable input errors (wrong OTP/CVV, limit hit, timed out). Prompt retry has real odds of success. |
| `network_error` | 3 | 5 min apart | Razorpay's own gateway-side hiccup, distinct from bank downtime — clears faster, so shorter spacing. |
| `unknown` | 0 | n/a | No classification rule matched — retrying blind risks acting on a failure mode we don't understand. Always escalate for human review rather than guess. |

## Category → decline code mapping

Matching uses `error_reason` as the primary key and `error_source` only as a
tiebreaker. `error_code` is deliberately **not** matched on: Razorpay only ever
sends `BAD_REQUEST_ERROR` or `GATEWAY_ERROR`, which is too coarse to
discriminate between categories. `error_description` is free text and unstable,
so it is not matched on either.

Rows are listed in match priority order. Anything that matches no row is
`unknown` — the classifier never guesses.

| # | `error_reason` | `error_source` | Category |
|---|---|---|---|
| 1 | `insufficient_funds` | any | `insufficient_funds` |
| 2 | `bank_technical_error` | any | `bank_downtime` |
| 3 | `card_declined` | any | `hard_decline` |
| 4 | `card_expired` | any | `hard_decline` |
| 5 | `payment_risk_check_failed` | any | `hard_decline` |
| 6 | `debit_instrument_blocked` | any | `hard_decline` |
| 7 | `authentication_failed` | any | `soft_decline` |
| 8 | `incorrect_cvv` | any | `soft_decline` |
| 9 | `payment_timed_out` | any | `soft_decline` |
| 10 | `gateway_technical_error` | `bank` | `bank_downtime` |
| 11 | `gateway_technical_error` | `gateway` | `network_error` |
| — | anything else | any | `unknown` |

Rows 10 and 11 are the only case where `error_source` affects the outcome. Every
other `error_reason` maps to exactly one category regardless of source, so the
row order above is descriptive rather than load-bearing — the nine direct
reasons are mutually exclusive.

`gateway_technical_error` with an `error_source` other than `bank` or `gateway`
falls through to `unknown` rather than defaulting to either branch. Guessing
here would mean either retrying a payment the bank has already rejected, or
declining to retry one that only failed in transit.
