package shop

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenRequest rebuilds the exact shop request the golden generator
// (open-combas-server-js scripts/gen-golden.mjs) fed the TypeScript server.
func goldenRequest(faction byte) []byte {
	request := make([]byte, HeaderLength)
	request[1] = 0x09 // type/magic bytes the client stamps on shop requests
	request[2], request[3] = 0x00, 0x01
	copy(request[4:], "00091234567890ABCDEF")
	gamertag := "PLAYER" + strings.Repeat(string(faction), 8)
	copy(request[12:], gamertag)
	request[0x1e] = faction
	return request
}

func loadGolden(t *testing.T, faction string) []byte {
	t.Helper()
	path := filepath.Join("testdata", "shop_"+faction+".hex")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	golden, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("golden %s: %v", path, err)
	}
	return golden
}

// TestBuildResponseMatchesGolden pins the Go shop reply byte-for-byte to the
// TypeScript server's reply for all three factions. Regenerate the goldens from
// the TypeScript sources (scripts/gen-golden.mjs) after any catalog change;
// a diff here means the Go port and the JS server disagree.
func TestBuildResponseMatchesGolden(t *testing.T) {
	for _, faction := range []string{"A", "B", "C"} {
		t.Run(faction, func(t *testing.T) {
			request := goldenRequest(faction[0])
			got := BuildResponse(request, CurrentLineup(faction))
			want := loadGolden(t, faction)

			if len(got) != len(want) {
				t.Fatalf("length: got %d, want %d", len(got), len(want))
			}
			if !bytes.Equal(got, want) {
				for i := range got {
					if got[i] != want[i] {
						start := i - 8
						if start < 0 {
							start = 0
						}
						end := i + 8
						if end > len(got) {
							end = len(got)
						}
						t.Fatalf("first diff at byte 0x%x: got %02x, want %02x\nwindow got:  %x\nwindow want: %x",
							i, got[i], want[i], got[start:end], want[start:end])
					}
				}
			}
		})
	}
}

// TestCurrentLineupLimits covers the packet-level rules: 160 rows max, 80 per
// section, and unknown factions serving nothing.
func TestCurrentLineupLimits(t *testing.T) {
	for _, faction := range []string{"A", "B", "C"} {
		lineup := CurrentLineup(faction)
		if len(lineup) > maxLineup {
			t.Fatalf("%s: lineup %d exceeds %d", faction, len(lineup), maxLineup)
		}
		if len(lineup) == 0 {
			t.Fatalf("%s: empty lineup", faction)
		}
		seen := make(map[uint32]struct{}, len(lineup))
		for _, record := range lineup {
			if _, dup := seen[record.PartID]; dup {
				t.Fatalf("%s: duplicate part id %#x", faction, record.PartID)
			}
			seen[record.PartID] = struct{}{}
		}
	}
	if lineup := CurrentLineup("D"); lineup != nil {
		t.Fatalf("faction D: expected no lineup, got %d rows", len(lineup))
	}
}

// TestBuildResponseEmpty covers a server answering before any catalog loads:
// the header still echoes the request and the sections stay empty.
func TestBuildResponseEmpty(t *testing.T) {
	request := goldenRequest('A')
	packet := BuildResponse(request, nil)
	if len(packet) != HeaderLength+sectionLength*2 {
		t.Fatalf("length: got %d, want %d", len(packet), HeaderLength+sectionLength*2)
	}
	if !bytes.Equal(packet[:HeaderLength], request) {
		t.Fatalf("header not echoed:\ngot  %x\nwant %x", packet[:HeaderLength], request)
	}
	for _, section := range []int{0, 1} {
		base := HeaderLength + section*sectionLength
		if count := packet[base+0x32]; count != 0 {
			t.Fatalf("section %d: count byte %d, want 0", section, count)
		}
	}
}

// TestAssetPartID pins the stem -> part id packing, including the WH/GA aliases.
func TestAssetPartID(t *testing.T) {
	cases := []struct {
		stem string
		id   uint32
	}{
		{"CK_CA001", 0x01010001},
		{"LG_TL001", 0x02010001},
		{"CP_MC001", 0x03010001},
		{"GE_GA001", 0x04010001},
		{"WH_HC001", 0x05010001}, // WH -> weapon family
		{"WP_HC001", 0x05010001}, // same id as WH_HC001
		{"AX_AM001", 0x06060001}, // AM is subtype 6 of family 6
		{"WJ_UJ001", 0x07010001},
	}
	for _, tc := range cases {
		id, ok := AssetPartID(tc.stem)
		if !ok || id != tc.id {
			t.Errorf("AssetPartID(%s) = %#x, %v; want %#x, true", tc.stem, id, ok, tc.id)
		}
	}
	// GA is a generator subtype, never a stem prefix: only WH and GA-as-generator
	// exist, so "GA_GA001" is not a stem the catalog uses.
	for _, stem := range []string{"GA_GA001", "ZZ_ZZ001", "CK_ZZ001", "CK_CA", "CK_CA001X"} {
		if id, ok := AssetPartID(stem); ok {
			t.Errorf("AssetPartID(%s) = %#x, true; want failure", stem, id)
		}
	}
}

// TestSpreadByPrefixInterleaves checks one family cannot monopolise the head of
// the lineup.
func TestSpreadByPrefixInterleaves(t *testing.T) {
	stems := []string{"AX_AM001", "AX_AM002", "AX_AM003", "CK_CA001", "CK_CA002"}
	spread := spreadByPrefix(stems)
	want := []string{"AX_AM001", "CK_CA001", "AX_AM002", "CK_CA002", "AX_AM003"}
	if fmt.Sprint(spread) != fmt.Sprint(want) {
		t.Fatalf("spread = %v, want %v", spread, want)
	}
}

// TestHoundStemsArePriced checks every hound preset part resolves to a catalog
// price, so a complete-hound preset can actually be bought.
func TestHoundStemsArePriced(t *testing.T) {
	for faction, stems := range houndStems {
		for _, stem := range stems {
			id, ok := AssetPartID(stem)
			if !ok {
				t.Fatalf("%s: hound stem %s has no part id", faction, stem)
			}
			if _, found := partByID(id); !found {
				t.Fatalf("%s: hound stem %s (id %#x) is not in the catalog", faction, stem, id)
			}
		}
	}
}
