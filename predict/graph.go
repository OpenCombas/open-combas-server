// Package predict is an OUT-OF-BAND season planner. It runs a fast in-memory Monte-Carlo of the war under a
// given reset downscale + calibration and reports how long (if ever) it takes to reach a single-nation
// outcome. It is NOT wired into the live server; it exists so downscale/behaviour can be swept offline.
//
// The war-progression kernel here is a faithful port of server.BattleApplier + WorldRepository (occupation
// accumulate -> crossover flip -> winner-takes-all + lock -> area owner by strategic value -> HQ fall /
// cascade / revival lockout; locked battlefields are inert). It is validated against the documented
// mechanics in predict_test.go. If BattleApplier's rules change, re-validate this port.
package predict

import "sort"

// nations, as the server encodes them.
const (
	natA byte = 'A' // Tarakia
	natB byte = 'B' // Morskoj
	natC byte = 'C' // Sal Kar
)

var nations = [3]byte{natA, natB, natC}

// capitalOf maps a capital area id to the nation whose HQ it is (hqDefaultNation in the server). Areas 1/2/3
// are the only capitals; every other area returns 0.
func capitalOf(area int32) byte {
	switch area {
	case 1:
		return natA
	case 2:
		return natB
	case 3:
		return natC
	}
	return 0
}

func capitalArea(n byte) int32 {
	switch n {
	case natA:
		return 1
	case natB:
		return 2
	case natC:
		return 3
	}
	return 0
}

// adjacencyEdges is the Neroimus strategic-map graph, decoded from the in-game debug "Area Data" display
// (road labels mAABB = an edge between area AA and area BB). This topology is FIXED client-side geography --
// the server never transmits it (confirmed: no world message carries adjacency), so it is a constant here.
var adjacencyEdges = [][2]int32{
	{2, 4}, {4, 6}, {6, 15}, {2, 16}, {16, 17}, {5, 16}, {6, 17}, {7, 17}, {8, 17}, {8, 19},
	{5, 19}, {18, 19}, {14, 15}, {13, 14}, {7, 13}, {1, 14}, {1, 11}, {10, 11}, {10, 22}, {21, 22},
	{18, 21}, {3, 18}, {3, 21}, {1, 12}, {9, 11}, {9, 12}, {9, 20}, {20, 22},
}

// buildAdjacency returns area -> sorted neighbour list.
func buildAdjacency() map[int32][]int32 {
	adj := map[int32]map[int32]bool{}
	for _, e := range adjacencyEdges {
		if adj[e[0]] == nil {
			adj[e[0]] = map[int32]bool{}
		}
		if adj[e[1]] == nil {
			adj[e[1]] = map[int32]bool{}
		}
		adj[e[0]][e[1]] = true
		adj[e[1]][e[0]] = true
	}
	out := map[int32][]int32{}
	for a, ns := range adj {
		lst := make([]int32, 0, len(ns))
		for n := range ns {
			lst = append(lst, n)
		}
		sort.Slice(lst, func(i, j int) bool { return lst[i] < lst[j] })
		out[a] = lst
	}
	return out
}

// hopDist returns BFS hop-distance from src to every reachable area.
func hopDist(adj map[int32][]int32, src int32) map[int32]int {
	d := map[int32]int{src: 0}
	q := []int32{src}
	for len(q) > 0 {
		u := q[0]
		q = q[1:]
		for _, v := range adj[u] {
			if _, ok := d[v]; !ok {
				d[v] = d[u] + 1
				q = append(q, v)
			}
		}
	}
	return d
}
