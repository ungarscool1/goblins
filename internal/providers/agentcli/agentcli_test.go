package agentcli

import (
	"testing"
	"time"
)

func TestRetryAtReadsOnlyStatedTimes(t *testing.T) {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"Claude AI usage limit reached|1772400000":   time.Unix(1772400000, 0).UTC(),
		`{"resetsAt": 1772400000}`:                   time.Unix(1772400000, 0).UTC(),
		`{"resets_at":1772400000}`:                   time.Unix(1772400000, 0).UTC(),
		"usage limit, resets in 2 hours":             now.Add(2 * time.Hour),
		"Please try again in 45s":                    now.Add(45 * time.Second),
		"please try again in 5 minutes":              now.Add(5 * time.Minute),
		`{"resets_in_seconds":120}`:                  now.Add(2 * time.Minute),
		"Retry-After: 30":                            now.Add(30 * time.Second),
		"You've hit your limit · resets 3pm (UTC)":   {},
		"usage limit reached":                        {},
		"the build resets everything in 3 databases": {},
	}
	for text, want := range cases {
		if got := RetryAt(text, now); !got.Equal(want) {
			t.Errorf("%q: got %v, want %v", text, got, want)
		}
	}
}
