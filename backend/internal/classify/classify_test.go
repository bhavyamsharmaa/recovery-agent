package classify

import "testing"

func TestClassify(t *testing.T) {
	tests := []struct {
		name        string
		errorReason string
		errorSource string
		want        Category
	}{
		{
			name:        "insufficient funds",
			errorReason: "insufficient_funds",
			errorSource: "customer",
			want:        CategoryInsufficientFunds,
		},
		{
			name:        "bank technical error",
			errorReason: "bank_technical_error",
			errorSource: "bank",
			want:        CategoryBankDowntime,
		},
		{
			name:        "card declined",
			errorReason: "card_declined",
			errorSource: "bank",
			want:        CategoryHardDecline,
		},
		{
			name:        "card expired",
			errorReason: "card_expired",
			errorSource: "bank",
			want:        CategoryHardDecline,
		},
		{
			name:        "payment risk check failed",
			errorReason: "payment_risk_check_failed",
			errorSource: "bank",
			want:        CategoryHardDecline,
		},
		{
			name:        "debit instrument blocked",
			errorReason: "debit_instrument_blocked",
			errorSource: "bank",
			want:        CategoryHardDecline,
		},
		{
			name:        "authentication failed",
			errorReason: "authentication_failed",
			errorSource: "customer",
			want:        CategorySoftDecline,
		},
		{
			name:        "incorrect cvv",
			errorReason: "incorrect_cvv",
			errorSource: "customer",
			want:        CategorySoftDecline,
		},
		{
			name:        "payment timed out",
			errorReason: "payment_timed_out",
			errorSource: "customer",
			want:        CategorySoftDecline,
		},
		// The two tiebreaker branches. No simulator scenario emits
		// gateway_technical_error with source=bank, so this table is that
		// branch's only coverage.
		{
			name:        "gateway technical error from bank is downtime",
			errorReason: "gateway_technical_error",
			errorSource: "bank",
			want:        CategoryBankDowntime,
		},
		{
			name:        "gateway technical error from gateway is network",
			errorReason: "gateway_technical_error",
			errorSource: "gateway",
			want:        CategoryNetworkError,
		},
		{
			name:        "unrecognised reason is unknown",
			errorReason: "moon_phase_unfavourable",
			errorSource: "customer",
			want:        CategoryUnknown,
		},
		// Guarding the two ways a near-miss could silently become a wrong
		// retry decision rather than an honest unknown.
		{
			name:        "gateway technical error from unknown source is unknown",
			errorReason: "gateway_technical_error",
			errorSource: "customer",
			want:        CategoryUnknown,
		},
		{
			name:        "empty input is unknown",
			errorReason: "",
			errorSource: "",
			want:        CategoryUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.errorReason, tt.errorSource)
			if got != tt.want {
				t.Errorf("Classify(%q, %q) = %q, want %q",
					tt.errorReason, tt.errorSource, got, tt.want)
			}
		})
	}
}

// The four direct reasons must not be affected by error_source. If a future
// change moves one of them behind a source check, this catches it.
func TestDirectReasonsIgnoreSource(t *testing.T) {
	sources := []string{"customer", "bank", "gateway", "issuer", ""}

	for reason, want := range byReason {
		for _, source := range sources {
			if got := Classify(reason, source); got != want {
				t.Errorf("Classify(%q, %q) = %q, want %q",
					reason, source, got, want)
			}
		}
	}
}
