package reset

import "testing"

// TestSeedBattlefields checks the reset layout invariants: one doc per (area,map), every battlefield 100%
// owned by exactly its area's default nation, and capacity = known capture points (else 25000).
func TestSeedBattlefields(t *testing.T) {
	bfs := seedBattlefields(1)

	// Total = sum of per-area counts (80 across the 22 areas).
	want := 0
	for a := 1; a < len(areaBattlefieldCount); a++ {
		want += int(areaBattlefieldCount[a])
	}
	if len(bfs) != want {
		t.Fatalf("got %d battlefields, want %d", len(bfs), want)
	}

	seen := map[[2]int32]bool{}
	dotSum := map[int32]int32{}
	for _, b := range bfs {
		key := [2]int32{b.AreaID, b.MapID}
		if seen[key] {
			t.Errorf("duplicate battlefield %v", key)
		}
		seen[key] = true
		dotSum[b.AreaID] += b.StrategicValue

		// Capacity matches the override table (or the default).
		if got := capacityFor(b.AreaID, b.MapID); b.Capacity != got {
			t.Errorf("area %d map %d capacity = %d, want %d", b.AreaID, b.MapID, b.Capacity, got)
		}

		// Exactly one nation holds 100% (== capacity); the other two are 0.
		occ := []int32{b.OccA, b.OccB, b.OccC}
		full := 0
		for i, o := range occ {
			switch o {
			case b.Capacity:
				full++
				if byte(i) != areaDefaultNation[b.AreaID] {
					t.Errorf("area %d map %d owned by nation %d, want %d", b.AreaID, b.MapID, i, areaDefaultNation[b.AreaID])
				}
			case 0:
			default:
				t.Errorf("area %d map %d has partial occupation %d", b.AreaID, b.MapID, o)
			}
		}
		if full != 1 {
			t.Errorf("area %d map %d: %d nations at 100%%, want exactly 1", b.AreaID, b.MapID, full)
		}
	}

	// Every area's strategic-value (orange dots) sums to 5.
	for area := int32(1); int(area) < len(areaBattlefieldCount); area++ {
		if dotSum[area] != areaDotTotal {
			t.Errorf("area %d dots sum to %d, want %d", area, dotSum[area], areaDotTotal)
		}
	}
}

// TestSeedBattlefieldsDownscale verifies the downscale factor divides capture points (and the occupation
// that fills them) while preserving the 100%-single-owner invariant and leaving strategic dots untouched.
func TestSeedBattlefieldsDownscale(t *testing.T) {
	base := seedBattlefields(1)
	scaled := seedBattlefields(20)
	if len(base) != len(scaled) {
		t.Fatalf("downscale changed battlefield count: %d vs %d", len(base), len(scaled))
	}
	for i := range base {
		b, s := base[i], scaled[i]
		wantCap := b.Capacity / 20
		if wantCap < 1 {
			wantCap = 1
		}
		if s.Capacity != wantCap {
			t.Errorf("area %d map %d capacity = %d, want %d (base %d / 20)", s.AreaID, s.MapID, s.Capacity, wantCap, b.Capacity)
		}
		// The owning nation still holds exactly 100% (== the downscaled capacity); others 0.
		occ := []int32{s.OccA, s.OccB, s.OccC}
		full := 0
		for _, o := range occ {
			switch o {
			case s.Capacity:
				full++
			case 0:
			default:
				t.Errorf("area %d map %d partial occupation %d after downscale", s.AreaID, s.MapID, o)
			}
		}
		if full != 1 {
			t.Errorf("area %d map %d: %d nations at 100%% after downscale, want 1", s.AreaID, s.MapID, full)
		}
		// Strategic value (orange dots) is NOT downscaled.
		if s.StrategicValue != b.StrategicValue {
			t.Errorf("area %d map %d dots changed by downscale: %d -> %d", s.AreaID, s.MapID, b.StrategicValue, s.StrategicValue)
		}
	}

	// downscale < 1 is treated as 1 (no change).
	if got := seedBattlefields(0); got[0].Capacity != base[0].Capacity {
		t.Errorf("downscale 0 changed capacity: %d vs %d", got[0].Capacity, base[0].Capacity)
	}
}

// TestKnownAssignments spot-checks a few nation/capacity values against the source data.
func TestKnownAssignments(t *testing.T) {
	bfs := seedBattlefields(1)
	get := func(area, mapID int32) *battlefield {
		for i := range bfs {
			if bfs[i].AreaID == area && bfs[i].MapID == mapID {
				return &bfs[i]
			}
		}
		return nil
	}

	// Xeres (1) = A at default 25000.
	if b := get(1, 1); b == nil || b.OccA != 25000 || b.OccB != 0 || b.OccC != 0 || b.Capacity != 25000 {
		t.Errorf("Xeres m1 = %+v", b)
	}
	// North Stanthorpe Bay (14,1) = A, 40000 capture points.
	if b := get(14, 1); b == nil || b.Capacity != 40000 || b.OccA != 40000 {
		t.Errorf("Stanthorpe m1 = %+v", b)
	}
	// Ostrov (2) = B.
	if b := get(2, 1); b == nil || b.OccB != b.Capacity || b.OccA != 0 {
		t.Errorf("Ostrov m1 = %+v", b)
	}
	// Tamala South Mine (22,4) = C, 28000.
	if b := get(22, 4); b == nil || b.Capacity != 28000 || b.OccC != 28000 || b.OccA != 0 || b.OccB != 0 {
		t.Errorf("Tamala m4 = %+v", b)
	}

	// Known dot distributions: Braidwood Cobar (12,1) = 2, Tajin Village Ruin (10,4) = 2.
	if b := get(12, 1); b == nil || b.StrategicValue != 2 {
		t.Errorf("Braidwood Cobar dots = %+v, want 2", b)
	}
	if b := get(10, 4); b == nil || b.StrategicValue != 2 {
		t.Errorf("Tajin Village Ruin dots = %+v, want 2", b)
	}
}
