package server

import (
	"testing"
	"time"
)

// The between-seasons gate: SeasonLocked tracks the clock, and while locked every served map record carries
// MapLockFlag=1 (deployment closed) regardless of its stored state.
func TestSeasonLockout(t *testing.T) {
	defer ApplySeasonStart(0) // don't leak global state to other tests

	ApplySeasonStart(time.Now().Add(time.Hour).Unix())
	if !SeasonLocked() {
		t.Fatal("a future season-start must read as locked")
	}
	maps := [6]AreaMapRecord{{MapID: 1}, {MapID: 2}, {MapID: 3}}
	st := newAreaInfoState([16]byte{}, [8]byte{}, 5, maps, 3, false)
	for i := 0; i < 6; i++ {
		if st.Maps[i].MapLockFlag != 1 {
			t.Errorf("locked season: map %d MapLockFlag = %d, want 1", i, st.Maps[i].MapLockFlag)
		}
	}

	// Past / zero start = active: the gate must NOT force the flag.
	ApplySeasonStart(time.Now().Add(-time.Hour).Unix())
	if SeasonLocked() {
		t.Fatal("a past season-start must read as unlocked")
	}
	st = newAreaInfoState([16]byte{}, [8]byte{}, 5, maps, 3, false)
	if st.Maps[0].MapLockFlag != 0 {
		t.Errorf("active season: MapLockFlag should stay 0, got %d", st.Maps[0].MapLockFlag)
	}

	ApplySeasonStart(0)
	if SeasonLocked() {
		t.Fatal("zero season-start = no lockout")
	}
}
