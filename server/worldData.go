package server

// Static world model shared by the area-map (code 196) and area-info (code 197) servers so the war
// map and the per-area battlefield data stay consistent. This is the seam the future dynamic layer
// (post-sortie mission outcomes -> occupation deltas, persisted in a DB) will replace.
//
// The world has 22 areas (1-based ids 1..22, matching mapdata/map/mNN_*). Each area has 3 or 4
// battlefields; the real counts come from that directory.

const battlefieldCapacity = 35000 // per-battlefield occupation capacity (the "/ NNNNN" denominator)

// areaBattlefieldCount[N] = number of battlefields in area N (index 1..22), from mapdata/map/mNN_*.
var areaBattlefieldCount = [23]byte{
	0,                            // index 0 unused (areas are 1-based)
	4, 4, 3, 3, 3, 4, 3, 4, 4, 4, // 1-10
	4, 4, 3, 3, 4, 3, 4, 4, 3, 4, // 11-20
	4, 4, // 21-22
}

var nationChars = [3]byte{'A', 'B', 'C'} // A=Tarakia, B=Morskoj, C=Sal Kar

func nationChar(i int) byte { return nationChars[((i%3)+3)%3] }

// areaBattlefields builds the per-battlefield occupation for an area deterministically, so the world
// is stable across repeated queries. Each area has a dominant nation that leads most of its
// battlefields, with one battlefield contested by another nation; occupation levels vary per
// battlefield within 0..battlefieldCapacity and the leader always holds the most.
func areaBattlefields(areaID byte) ([6]AreaMapRecord, byte) {
	var maps [6]AreaMapRecord
	n := int(areaID)
	if n < 1 || n >= len(areaBattlefieldCount) {
		return maps, 0
	}
	count := areaBattlefieldCount[n]
	dominant := (n - 1) % 3 // area's controlling nation rotates across the map

	for j := byte(0); j < count; j++ {
		leader := dominant
		if j == count-1 {
			leader = (dominant + 1 + (n % 2)) % 3 // last battlefield held by a rival nation
		}

		// The three nations partition a single capacity pool EXACTLY (sum == capacity), so the
		// combined occupation neither overflows nor underflows the bar denominator. The leader takes
		// the largest slice, then the remainder splits between the other two with the last nation
		// taking the exact remainder.
		cap := int32(battlefieldCapacity)
		lead := cap * int32(50+(n*7+int(j)*11)%25) / 100 // 50..74% of capacity
		rem := cap - lead
		second := rem * int32(55+(n*5+int(j)*13)%30) / 100 // 55..84% of the remainder
		third := rem - second                              // exact remainder -> lead+second+third == cap

		var occ [3]int32
		occ[leader] = lead
		occ[(leader+1)%3] = second
		occ[(leader+2)%3] = third

		maps[j] = AreaMapRecord{
			MapID:              int16(j + 1),
			ControllingFaction: nationChar(leader),
			Capacity:           battlefieldCapacity, // bar denominator (max occupation pool)
			ControlLevel:       lead,                // bar numerator (leader's slice of the pool)
			OccupationPoints:   int32(1 + int(j)%4), // strategic value -> orange dots (1..4)
			InvasionA:          occ[0],
			InvasionB:          occ[1],
			InvasionC:          occ[2],
		}
	}
	return maps, count
}

// areaControlSummary derives the area-level control for the area-map (196) server from the same
// battlefields, so the war-map owner flag and the per-area battlefield data agree. The owner is the
// nation leading the most battlefields; the per-nation points are the summed battlefield occupation.
func areaControlSummary(areaID byte) (owner byte, pointsA, pointsB, pointsC int32) {
	maps, count := areaBattlefields(areaID)
	if count == 0 {
		return 'A', 0, 0, 0
	}
	// Per-nation OCCUPATION POINTS (the orange dots) = the strategic value of the battlefields each nation
	// CONTROLS -- NOT the capture-level occupation, which only decides who controls a battlefield and must
	// not surface to the area (its magnitude saturates the small dot display). Mirrors areaSummaryFrom (the
	// live/Mongo path); occTotal is only the owner tiebreak.
	var points, occTotal [3]int32
	for j := byte(0); j < count; j++ {
		occTotal[0] += maps[j].InvasionA
		occTotal[1] += maps[j].InvasionB
		occTotal[2] += maps[j].InvasionC
		if maps[j].ControlLevel <= 0 {
			continue
		}
		switch maps[j].ControllingFaction {
		case 'A':
			points[0] += maps[j].OccupationPoints
		case 'B':
			points[1] += maps[j].OccupationPoints
		case 'C':
			points[2] += maps[j].OccupationPoints
		}
	}
	best := 0
	for i := 1; i < 3; i++ {
		if points[i] > points[best] || (points[i] == points[best] && occTotal[i] > occTotal[best]) {
			best = i
		}
	}
	return nationChar(best), points[0], points[1], points[2]
}
