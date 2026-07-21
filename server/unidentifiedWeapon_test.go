package server

import "testing"

// TestWeaponSitesMatchNewsText pins each nation's row triple and deployment site against the FMG text of
// WorldSituationInfoNewsParam rows 47-52 / 81-83. These rows have NO placeholder tokens -- the battlefield
// name is baked into the story -- so choosing the row IS choosing the location, and a mismatched row would
// publish a story naming the wrong nation and the wrong battlefield with nothing to flag it.
func TestWeaponSitesMatchNewsText(t *testing.T) {
	want := []struct {
		nation                       byte
		name, battlefield            string
		areaID, mapID                int32
		appear, destroyed, withdrawn int32
	}{
		{'A', "Tarakia", "Wakool", 11, 4, 47, 50, 81},
		{'B', "Morskoj", "East Salma Woods", 17, 4, 48, 51, 82},
		{'C', "Sal Kar", "South Cemo Oil Field", 18, 4, 49, 52, 83},
	}
	for _, w := range want {
		site, ok := UnidentifiedWeaponSiteFor(w.nation)
		if !ok {
			t.Fatalf("no site for nation %q", string(w.nation))
		}
		if site.NationName != w.name || site.Battlefield != w.battlefield {
			t.Errorf("nation %q = %s/%s, want %s/%s", string(w.nation), site.NationName, site.Battlefield, w.name, w.battlefield)
		}
		if site.AreaID != w.areaID || site.MapID != w.mapID {
			t.Errorf("%s battlefield = %d/%d, want %d/%d", w.name, site.AreaID, site.MapID, w.areaID, w.mapID)
		}
		for phase, wantRow := range map[UnidentifiedWeaponPhase]int32{
			WeaponAppears: w.appear, WeaponDestroyed: w.destroyed, WeaponWithdrawn: w.withdrawn,
		} {
			got, ok := WeaponPhaseRow(w.nation, phase)
			if !ok || got != wantRow {
				t.Errorf("%s %s row = %d (ok=%v), want %d", w.name, phase, got, ok, wantRow)
			}
		}
	}
}

// Rows must not be shared between nations or phases: every one of the nine is a distinct story.
func TestWeaponRowsAreUnique(t *testing.T) {
	seen := map[int32]string{}
	for _, code := range []byte{'A', 'B', 'C'} {
		for _, phase := range []UnidentifiedWeaponPhase{WeaponAppears, WeaponDestroyed, WeaponWithdrawn} {
			row, _ := WeaponPhaseRow(code, phase)
			label := string(code) + "/" + phase.String()
			if prev, dup := seen[row]; dup {
				t.Errorf("row %d used by both %s and %s", row, prev, label)
			}
			seen[row] = label
		}
	}
	if len(seen) != 9 {
		t.Errorf("got %d distinct rows, want 9", len(seen))
	}
}

func TestUnidentifiedWeaponSiteForIsCaseInsensitive(t *testing.T) {
	upper, ok1 := UnidentifiedWeaponSiteFor('B')
	lower, ok2 := UnidentifiedWeaponSiteFor('b')
	if !ok1 || !ok2 || upper.Battlefield != lower.Battlefield {
		t.Errorf("case handling: %v/%v %q vs %q", ok1, ok2, upper.Battlefield, lower.Battlefield)
	}
	if _, ok := UnidentifiedWeaponSiteFor('D'); ok {
		t.Error("nation 'D' should not resolve")
	}
}

func TestParseWeaponPhase(t *testing.T) {
	cases := map[string]UnidentifiedWeaponPhase{
		"appear": WeaponAppears, "appears": WeaponAppears, "APPEAR": WeaponAppears,
		"destroy": WeaponDestroyed, "destroyed": WeaponDestroyed,
		"withdraw": WeaponWithdrawn, "withdrawn": WeaponWithdrawn,
	}
	for in, want := range cases {
		if got, ok := ParseWeaponPhase(in); !ok || got != want {
			t.Errorf("ParseWeaponPhase(%q) = %v (ok=%v), want %v", in, got, ok, want)
		}
	}
	if _, ok := ParseWeaponPhase("deploy"); ok {
		t.Error(`"deploy" should not parse -- it is not one of the three phases`)
	}
}
