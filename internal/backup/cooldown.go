package backup

import (
	"fmt"
	"time"
)

// ErrProviderCooldown is returned by a plugin's Upload or Verify when a
// destination has hit a provider-wide rate/quota limit that will not clear until
// a known time (e.g. Google Photos' daily upload quota, which blocks ALL uploads
// until ~midnight Pacific). It is NOT an upload failure: the Manager reacts by
// returning the job to pending untouched (no retry increment) and putting the
// whole PROVIDER into cooldown for RetryAfter, so workers stop claiming that
// provider's jobs until it clears. Reason is a short human-readable cause used in
// logs and the Backup Queue cooldown banner.
type ErrProviderCooldown struct {
	// RetryAfter is how long from now the provider should be considered cooling.
	RetryAfter time.Duration
	// Reason is a short human-readable cause (e.g. "Google Photos daily upload
	// quota reached").
	Reason string
}

// Error implements error.
func (e *ErrProviderCooldown) Error() string {
	return fmt.Sprintf("provider cooldown: %s (retry after %s)", e.Reason, e.RetryAfter.Round(time.Second))
}
