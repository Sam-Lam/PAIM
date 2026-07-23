package version

import "testing"

// TestFullFallbacks exercises Full()'s graceful degradation when the build
// stamps are absent or only partially set. The Commit/Date package vars are
// restored after each case so the test does not leak state.
func TestFullFallbacks(t *testing.T) {
	origCommit, origDate := Commit, Date
	t.Cleanup(func() { Commit, Date = origCommit, origDate })

	cases := []struct {
		name         string
		commit, date string
		want         string
	}{
		{"both unset", "", "", Version + " (dev)"},
		{"both set", "abc1234", "2026-07-22", Version + " (abc1234, 2026-07-22)"},
		{"commit only", "abc1234", "", Version + " (abc1234, dev)"},
		{"date only", "", "2026-07-22", Version + " (dev, 2026-07-22)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			Commit, Date = tc.commit, tc.date
			if got := Full(); got != tc.want {
				t.Errorf("Full() = %q, want %q", got, tc.want)
			}
		})
	}
}
