package rclone

import (
	"testing"
	"time"
)

// TestClassifyCooldownFixtures exercises the stderr classifier with realistic
// rclone error output for Google Photos' daily quota, transient rate limits, and
// unrelated failures.
func TestClassifyCooldownFixtures(t *testing.T) {
	// 07:00 America/Los_Angeles on 2026-07-23 == 14:00 UTC.
	now := time.Date(2026, 7, 23, 14, 0, 0, 0, time.UTC)
	base := untilNextPacificMidnight(now) // ~17h to next Pacific midnight

	cases := []struct {
		name       string
		stderr     string
		wantCool   bool
		wantDaily  bool // true => RetryAfter in [base, base+jitter]; false => 10m
		wantReason string
	}{
		{
			name:       "daily quota exceeded",
			stderr:     "2026/07/23 ERROR : a.jpg: Failed to copy: googleapi: Error 429: Quota exceeded for quota metric 'All requests' and limit 'Requests per day', quotaExceeded",
			wantCool:   true,
			wantDaily:  true,
			wantReason: "Google Photos daily upload quota reached",
		},
		{
			name:      "resource exhausted per day",
			stderr:    "Failed to copy: rpc error: code = ResourceExhausted desc = RESOURCE_EXHAUSTED: Quota exceeded ... limit per day",
			wantCool:  true,
			wantDaily: true,
		},
		{
			name:       "rate limit exceeded (transient)",
			stderr:     "2026/07/23 ERROR : googleapi: Error 429: User rate limit exceeded, rateLimitExceeded",
			wantCool:   true,
			wantDaily:  false,
			wantReason: "provider rate limit reached (429)",
		},
		{
			name:      "resource exhausted concurrent (no per day)",
			stderr:    "rpc error: code = ResourceExhausted desc = RESOURCE_EXHAUSTED: concurrent writes",
			wantCool:  true,
			wantDaily: false,
		},
		{
			name:     "unrelated failure",
			stderr:   "Failed to copy: permission denied",
			wantCool: false,
		},
		{
			name:     "not found",
			stderr:   "directory not found",
			wantCool: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cd, ok := classifyCooldown(tc.stderr, now)
			if ok != tc.wantCool {
				t.Fatalf("ok = %v, want %v", ok, tc.wantCool)
			}
			if !ok {
				return
			}
			if tc.wantDaily {
				if cd.RetryAfter < base || cd.RetryAfter > base+dailyQuotaJitter {
					t.Fatalf("daily RetryAfter = %s, want in [%s, %s]", cd.RetryAfter, base, base+dailyQuotaJitter)
				}
			} else {
				if cd.RetryAfter != shortRateCooldown {
					t.Fatalf("rate RetryAfter = %s, want %s", cd.RetryAfter, shortRateCooldown)
				}
			}
			if tc.wantReason != "" && cd.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", cd.Reason, tc.wantReason)
			}
		})
	}
}

// TestUntilNextPacificMidnight sanity-checks the daily reset computation.
func TestUntilNextPacificMidnight(t *testing.T) {
	now := time.Date(2026, 7, 23, 14, 0, 0, 0, time.UTC) // 07:00 Pacific
	d := untilNextPacificMidnight(now)
	if d <= 0 || d > 24*time.Hour {
		t.Fatalf("duration = %s, want within (0, 24h]", d)
	}
	// From 07:00 Pacific, midnight is 17h away.
	if d < 16*time.Hour || d > 18*time.Hour {
		t.Fatalf("duration = %s, want ~17h", d)
	}
}
