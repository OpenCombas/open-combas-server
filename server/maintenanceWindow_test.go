package server

import (
	"testing"
	"time"
)

// clientMaintenanceState mirrors Release.xex sub_823B5FC8 so the tests below assert against the client's
// actual decision rather than our intent. Returned values match the flags it sets.
func clientMaintenanceState(now, start, end time.Time) string {
	switch {
	case !now.Before(start) && !now.After(end):
		return "in-maintenance" // +2904 -> boots players to the title screen
	case !now.Before(start):
		return "ended" // +2908 -> announce gate returns early: SILENT
	case now.Add(15 * time.Minute).After(start):
		return "approaching" // +2900 -> countdown popup
	default:
		return "future" // all flags 0 -> gate falls through: DATED POPUP
	}
}

func TestSilentPastWindowProducesEndedState(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	start, end := silentPastWindow(now)

	if !start.Before(end) {
		t.Errorf("start %v is not before end %v", start, end)
	}
	if !end.Before(now) {
		t.Errorf("end %v is not in the past relative to %v", end, now)
	}
	if got := clientMaintenanceState(now, start, end); got != "ended" {
		t.Errorf("default window yields client state %q, want %q -- any other state shows a popup", got, "ended")
	}
}

// The margin exists to absorb skew between our clock and the console's server-synced one. If `now` drifted
// to before `end`, the client would read "in-maintenance" and boot every player to the title screen, so
// assert the default keeps a substantial buffer.
func TestSilentPastWindowToleratesClockSkew(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	start, end := silentPastWindow(now)

	for _, skew := range []time.Duration{-12 * time.Hour, -time.Hour, 0, time.Hour, 12 * time.Hour} {
		skewed := now.Add(skew)
		if got := clientMaintenanceState(skewed, start, end); got != "ended" {
			t.Errorf("with %v clock skew the client reads %q, want %q", skew, got, "ended")
		}
	}
}

// Documents the state table this whole design rests on -- in particular that a FUTURE window is the one
// setting guaranteed to nag, which is the bug this replaced.
func TestClientMaintenanceStateTable(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		start, end time.Duration // relative to now
		want       string
	}{
		{"far future (the old 120h/140h setting)", 120 * time.Hour, 140 * time.Hour, "future"},
		{"near future, just outside the announce threshold", 20 * time.Minute, 2 * time.Hour, "future"},
		{"inside the 15 minute announce threshold", 10 * time.Minute, 2 * time.Hour, "approaching"},
		{"currently in maintenance", -time.Hour, time.Hour, "in-maintenance"},
		{"resetting config (-12h/+24h)", -12 * time.Hour, 24 * time.Hour, "in-maintenance"},
		{"fully elapsed", -48 * time.Hour, -24 * time.Hour, "ended"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clientMaintenanceState(now, now.Add(tc.start), now.Add(tc.end))
			if got != tc.want {
				t.Errorf("state = %q, want %q", got, tc.want)
			}
		})
	}
}
