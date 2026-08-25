// Package classify maps a failed payment's error fields to a failure category.
//
// The mapping is transcribed from the "Category → decline code mapping" section
// of docs/taxonomy.md. That document is the source of truth: change it first,
// then mirror the change here.
package classify

type Category string

const (
	CategoryInsufficientFunds Category = "insufficient_funds"
	CategoryBankDowntime      Category = "bank_downtime"
	CategoryHardDecline       Category = "hard_decline"
	CategorySoftDecline       Category = "soft_decline"
	CategoryNetworkError      Category = "network_error"
	CategoryUnknown           Category = "unknown"
)

// reasonGatewayTechnical is the one error_reason whose category depends on
// error_source, so it is handled by the tiebreaker rather than byReason.
const reasonGatewayTechnical = "gateway_technical_error"

// byReason holds taxonomy.md rows 1-9: reasons that map to exactly one category
// regardless of error_source.
var byReason = map[string]Category{
	"insufficient_funds":        CategoryInsufficientFunds,
	"bank_technical_error":      CategoryBankDowntime,
	"card_declined":             CategoryHardDecline,
	"card_expired":              CategoryHardDecline,
	"payment_risk_check_failed": CategoryHardDecline,
	"debit_instrument_blocked":  CategoryHardDecline,
	"authentication_failed":     CategorySoftDecline,
	"incorrect_cvv":             CategorySoftDecline,
	"payment_timed_out":         CategorySoftDecline,
}

// Classify maps error_reason (primary) and error_source (tiebreaker)
// to a Category. error_code is NOT used for matching — it's too
// coarse (only BAD_REQUEST_ERROR / GATEWAY_ERROR) to discriminate.
// Return CategoryUnknown if nothing matches — never guess.
func Classify(errorReason, errorSource string) Category {
	// Rows 10-11: the only case where error_source changes the answer. A gateway
	// technical error means the bank rejected it (do not retry the same way) or
	// it never arrived (safe to retry) — opposite actions, so an unrecognised
	// source falls through to unknown rather than picking a side.
	if errorReason == reasonGatewayTechnical {
		switch errorSource {
		case "bank":
			return CategoryBankDowntime
		case "gateway":
			return CategoryNetworkError
		default:
			return CategoryUnknown
		}
	}

	// Rows 1-9.
	if c, ok := byReason[errorReason]; ok {
		return c
	}

	return CategoryUnknown
}
