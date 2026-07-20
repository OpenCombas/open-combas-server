package server

import "testing"

func TestGradeFromRenown(t *testing.T) {
	cases := []struct {
		name   string
		renown int32
		want   byte
	}{
		{"never fought", 0, squadGradeMin},
		{"negative (corrupt/underflow)", -500, squadGradeMin},
		{"one win, below the first rung", 150, squadGradeMin},
		{"exactly grade 2", 300, 2},
		{"just under grade 3", 499, 2},
		{"exactly grade 3", 500, 3},
		{"mid ladder", 2500, 6},
		{"just under grade 10", 20999, 9},
		{"exactly grade 10", 21000, 10},
		{"just under the top", 99999, 12},
		{"exactly the top", 100000, squadGradeMax},
		{"far past the top stays capped", 5000000, squadGradeMax},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gradeFromRenown(tc.renown); got != tc.want {
				t.Errorf("gradeFromRenown(%d) = %d, want %d", tc.renown, got, tc.want)
			}
		})
	}
}

// The ladder must be ordered highest-first (gradeFromRenown returns the first threshold met), strictly
// descending in both grade and renown, renderable as FMG 5700+idx, and bottom out at 0 so every squad has
// a grade.
func TestSquadGradeLadderWellFormed(t *testing.T) {
	if len(squadGradeLadder) == 0 {
		t.Fatal("no grade thresholds defined")
	}
	prevGrade := byte(squadGradeMax + 1)
	prevRenown := int32(1<<31 - 1)
	for i, th := range squadGradeLadder {
		if th.Grade < squadGradeMin || th.Grade > squadGradeMax {
			t.Errorf("rung %d: grade %d outside %d..%d", i, th.Grade, squadGradeMin, squadGradeMax)
		}
		if th.Grade >= prevGrade {
			t.Errorf("rung %d: grade %d not descending (previous %d)", i, th.Grade, prevGrade)
		}
		if th.Renown >= prevRenown {
			t.Errorf("rung %d: renown %d not descending (previous %d)", i, th.Renown, prevRenown)
		}
		prevGrade, prevRenown = th.Grade, th.Renown
	}
	last := squadGradeLadder[len(squadGradeLadder)-1]
	if last.Grade != squadGradeMin || last.Renown != 0 {
		t.Errorf("last rung is grade %d at %d renown; must be grade %d at 0 so every squad has a grade",
			last.Grade, last.Renown, squadGradeMin)
	}
}

// Every grade in squadGradeMin..squadGradeMax must be reachable -- a gap would mean a rung nothing can
// ever display.
func TestEveryGradeReachable(t *testing.T) {
	seen := make(map[byte]bool, len(squadGradeLadder))
	for _, th := range squadGradeLadder {
		seen[th.Grade] = true
	}
	for g := byte(squadGradeMin); g <= squadGradeMax; g++ {
		if !seen[g] {
			t.Errorf("grade %d has no threshold and can never be displayed", g)
		}
	}
}

func TestDepartingMemberShare(t *testing.T) {
	cases := []struct {
		name   string
		bucket int32
		before int
		want   int32
	}{
		{"half of a two-man squad", 1000, 2, 500},
		{"quarter of a four-man squad", 1000, 4, 250},
		{"rounds down in the squad's favour", 1000, 3, 333},
		{"sole member disbands, no debit", 1000, 1, 0},
		{"empty roster", 1000, 0, 0},
		{"never earned anything", 0, 4, 0},
		{"negative bucket is not amplified", -500, 4, 0},
		{"full roster", 100000, 20, 5000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := departingMemberShare(tc.bucket, tc.before); got != tc.want {
				t.Errorf("departingMemberShare(%d, %d) = %d, want %d", tc.bucket, tc.before, got, tc.want)
			}
		})
	}
}

// Renown-Per-Member (the 1262 KBN-3 ranking) must survive a departure unchanged -- that invariant is the
// reason the debit is an equal share rather than any other fraction. Integer rounding makes it approximate,
// so assert it does not drift by more than the rounding error.
func TestDepartingMemberKeepsRenownPerMemberStable(t *testing.T) {
	for _, n := range []int{2, 3, 4, 7, 12, 20} {
		for _, total := range []int32{300, 5000, 100000} {
			before := float64(total) / float64(n)
			after := float64(total-departingMemberShare(total, n)) / float64(n-1)
			if diff := after - before; diff < -1 || diff > 1 {
				t.Errorf("n=%d total=%d: renown-per-member moved %.3f -> %.3f (drift %.3f)",
					n, total, before, after, diff)
			}
		}
	}
}
