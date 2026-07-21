package server

import "testing"

func TestGradeFromRenown(t *testing.T) {
	// Boundaries come from the retail Squad Rank / Renown Required table (see squadGradeLadder). Each
	// tier is checked at its exact threshold and one renown below it, because an off-by-one here silently
	// mislabels a squad's visible standing rather than failing anything.
	cases := []struct {
		name   string
		renown int32
		want   byte
	}{
		{"never fought", 0, squadGradeMin},
		{"negative (corrupt/underflow)", -500, squadGradeMin},
		{"a few wins, still Rookie", 900, squadGradeMin},
		{"just under Rookie+", 5999, squadGradeMin},
		{"exactly Rookie+", 6000, 2},
		{"just under Rookie++", 11999, 2},
		{"exactly Rookie++", 12000, 3},
		{"the tier jump: still Rookie++ at 29999", 29999, 3},
		{"exactly Regular", 30000, 4},
		{"exactly Regular+", 36000, 5},
		{"exactly Regular++", 42000, 6},
		{"just under Professional", 59999, 6},
		{"exactly Professional", 60000, 7},
		{"exactly Professional+", 66000, 8},
		{"exactly Professional++", 72000, 9},
		{"just under Master", 89999, 9},
		{"exactly Master", 90000, 10},
		{"exactly Master+", 96000, 11},
		{"exactly Master++", 102000, 12},
		{"just under Legend", 119999, 12},
		{"exactly Legend", 120000, squadGradeMax},
		{"far past Legend stays capped", 5000000, squadGradeMax},
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

func TestDebitBucket(t *testing.T) {
	const season = "0014"
	mk := func(total, seasonVal int32) StatBucket {
		return StatBucket{Total: total, BySeason: map[string]int32{season: seasonVal}}
	}
	cases := []struct {
		name             string
		bucket           StatBucket
		amount           int32
		wantTot, wantSea int32
	}{
		{"plain debit", mk(1000, 400), 100, 100, 100},
		// The buckets diverge for a squad that earned in an earlier season: the season sub-total must not be
		// driven negative just because the running total can absorb the full amount.
		{"season bucket smaller than the debit", mk(1000, 30), 100, 100, 30},
		{"total smaller than the debit", mk(40, 40), 100, 40, 40},
		{"nothing banked", mk(0, 0), 100, 0, 0},
		{"already negative is not amplified", mk(-50, -50), 100, 0, 0},
		{"zero amount", mk(1000, 400), 0, 0, 0},
		{"negative amount is ignored", mk(1000, 400), -20, 0, 0},
		{"exact drain", mk(100, 100), 100, 100, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotTot, gotSea := debitBucket(tc.bucket, season, tc.amount)
			if gotTot != tc.wantTot || gotSea != tc.wantSea {
				t.Errorf("debitBucket(%+v, %d) = (%d,%d), want (%d,%d)",
					tc.bucket, tc.amount, gotTot, gotSea, tc.wantTot, tc.wantSea)
			}
		})
	}
}

// A bucket can never be pushed below zero, whatever sequence of debits is applied -- a negative would sort
// the squad below squads that never played.
func TestDebitBucketNeverGoesNegative(t *testing.T) {
	const season = "0014"
	b := StatBucket{Total: 250, BySeason: map[string]int32{season: 90}}
	for i := 0; i < 20; i++ {
		dTot, dSea := debitBucket(b, season, 40)
		b.Total -= dTot
		b.BySeason[season] -= dSea
		if b.Total < 0 || b.BySeason[season] < 0 {
			t.Fatalf("iteration %d drove a bucket negative: total=%d season=%d", i, b.Total, b.BySeason[season])
		}
	}
	if b.Total != 0 || b.BySeason[season] != 0 {
		t.Errorf("after repeated debits buckets = (%d,%d), want (0,0)", b.Total, b.BySeason[season])
	}
}
