package server

import (
	"testing"
	"time"
)

// clientMaintenanceState mirrors the client's flag derivation (Release.xex sub_823B5FC8) so these tests
// assert against its actual decision rather than our intent.
func clientMaintenanceState(now, start, end time.Time) string {
	switch {
	case !now.Before(start) && !now.After(end):
		return "in-maintenance"
	case !now.Before(start):
		return "ended"
	case now.Add(15 * time.Minute).After(start):
		return "approaching"
	default:
		return "future"
	}
}

// clientOnlineAvailable mirrors sub_82151700: the client treats BOTH "in maintenance" and "ended" as the
// server being offline. This is the predicate that made a past-dated window worse than the popup it
// silenced.
func clientOnlineAvailable(state string) bool {
	return state != "in-maintenance" && state != "ended"
}

func TestDefaultWindowKeepsServerAvailable(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	start, end := defaultFutureWindow(now)

	state := clientMaintenanceState(now, start, end)
	if state != "future" {
		t.Errorf("default window yields %q, want %q", state, "future")
	}
	if !clientOnlineAvailable(state) {
		t.Errorf("default window marks the server UNAVAILABLE (state %q) -- this takes the whole server down", state)
	}
	if !start.After(now.Add(15 * time.Minute)) {
		t.Errorf("default start %v is inside the ~15 min announce threshold; the countdown popup would fire", start)
	}
}

// The window must stay in the future across any plausible skew between our clock and the console's
// server-synced one. Drifting into "ended" would mark the server offline.
func TestDefaultWindowToleratesClockSkew(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	start, end := defaultFutureWindow(now)

	for _, skew := range []time.Duration{-48 * time.Hour, -time.Hour, 0, time.Hour, 48 * time.Hour} {
		state := clientMaintenanceState(now.Add(skew), start, end)
		if !clientOnlineAvailable(state) {
			t.Errorf("with %v skew the client reads %q -- server unavailable", skew, state)
		}
	}
}

// Documents the full state table, including the two states that take the server offline. The "past" row is
// the corrected finding: it silences the announce but costs availability.
func TestClientMaintenanceStateTable(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name          string
		start, end    time.Duration // relative to now
		wantState     string
		wantAvailable bool
	}{
		{"default (30d out)", defaultWindowStartIn, defaultWindowEndIn, "future", true},
		{"near future, outside the threshold", 20 * time.Minute, 2 * time.Hour, "future", true},
		{"inside the 15 min threshold", 10 * time.Minute, 2 * time.Hour, "approaching", true},
		{"currently in maintenance", -time.Hour, time.Hour, "in-maintenance", false},
		{"resetting config (-12h/+24h)", -12 * time.Hour, 24 * time.Hour, "in-maintenance", false},
		{"fully elapsed -- silent but OFFLINE", -48 * time.Hour, -24 * time.Hour, "ended", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := clientMaintenanceState(now, now.Add(tc.start), now.Add(tc.end))
			if state != tc.wantState {
				t.Errorf("state = %q, want %q", state, tc.wantState)
			}
			if got := clientOnlineAvailable(state); got != tc.wantAvailable {
				t.Errorf("available = %v, want %v", got, tc.wantAvailable)
			}
		})
	}
}
