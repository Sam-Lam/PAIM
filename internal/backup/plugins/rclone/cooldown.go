package rclone

import (
	"math/rand"
	"strings"
	"time"

	"github.com/Sam-Lam/PAIM/internal/backup"
)

// dailyQuotaJitter caps the random extra delay added on top of "until next
// Pacific midnight" so a fleet of uploads does not all resume at the same instant.
const dailyQuotaJitter = 15 * time.Minute

// shortRateCooldown is the cooldown for a transient rate/concurrency limit (a 429
// or RESOURCE_EXHAUSTED that is NOT a daily-quota exhaustion).
const shortRateCooldown = 10 * time.Minute

// classifyCooldown inspects rclone stderr for the provider quota/rate patterns
// Google Photos (and similar backends) surface — quotaExceeded, rateLimitExceeded,
// RESOURCE_EXHAUSTED, HTTP 429 — and, when matched, returns the cooldown the
// destination should observe:
//
//   - a daily-quota exhaustion (quotaExceeded / RESOURCE_EXHAUSTED that mentions a
//     per-day limit) blocks ALL uploads until ~midnight America/Los_Angeles, so the
//     cooldown is "until next Pacific midnight" plus up to 15 minutes of jitter;
//   - a concurrent-write / rate limit (429, rateLimitExceeded, or a RESOURCE_
//     EXHAUSTED without a per-day mention) is transient, so the cooldown is 10
//     minutes.
//
// It returns ok=false for stderr that matches none of the patterns. now is passed
// in so the daily computation is deterministic under test.
func classifyCooldown(stderr string, now time.Time) (*backup.ErrProviderCooldown, bool) {
	s := strings.ToLower(stderr)

	hasQuota := strings.Contains(s, "quotaexceeded") ||
		strings.Contains(s, "quota exceeded") ||
		strings.Contains(s, "resource_exhausted")
	perDay := strings.Contains(s, "per day") ||
		strings.Contains(s, "perday") ||
		strings.Contains(s, "per-day") ||
		strings.Contains(s, "daily")

	if hasQuota && perDay {
		base := untilNextPacificMidnight(now)
		jitter := time.Duration(rand.Int63n(int64(dailyQuotaJitter) + 1))
		return &backup.ErrProviderCooldown{
			RetryAfter: base + jitter,
			Reason:     "Google Photos daily upload quota reached",
		}, true
	}

	hasRate := hasQuota ||
		strings.Contains(s, "ratelimitexceeded") ||
		strings.Contains(s, "rate limit") ||
		strings.Contains(s, "userratelimitexceeded") ||
		strings.Contains(s, "429")
	if hasRate {
		return &backup.ErrProviderCooldown{
			RetryAfter: shortRateCooldown,
			Reason:     "provider rate limit reached (429)",
		}, true
	}
	return nil, false
}

// untilNextPacificMidnight returns the duration from now until the next midnight
// in America/Los_Angeles (the timezone Google Photos' daily quota resets in). If
// the zoneinfo database is unavailable it falls back to a fixed -08:00 offset,
// which is close enough for a quota that is refreshed once a day.
func untilNextPacificMidnight(now time.Time) time.Duration {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		loc = time.FixedZone("PST", -8*60*60)
	}
	n := now.In(loc)
	next := time.Date(n.Year(), n.Month(), n.Day()+1, 0, 0, 0, 0, loc)
	d := next.Sub(now)
	if d < 0 {
		d = 0
	}
	return d
}
