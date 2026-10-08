package shop

import (
	"encoding/binary"
)

// Shop packet layout, as the Xbox 360 client expects it. Mirrors
// src/services/shop-response.ts of open-combas-server-js.
const (
	// HeaderLength is the copied request prefix that makes the client accept
	// the reply (session/sequence fields).
	HeaderLength = 0x20
	// sectionLength is one half of the buy list. The client keeps the section
	// name from the first half.
	sectionLength = 0x3f4
	// recordOffset is where the parts start in a section. Before it: the
	// section name, five "new part" ids and the one hit part.
	recordOffset = 0x34
	// recordSize is one part: id (u32), stock (u16), price (u16), flag (u8).
	recordSize = 12
	// maxPerSection is 80. Part 81 would land on the next half.
	maxPerSection = (sectionLength - recordOffset) / recordSize
	// maxLineup is 160. Part 161 would overwrite the data that follows the list.
	maxLineup = maxPerSection * 2
	// normalSalesStock keeps a part on the buy list. It is not a quantity:
	// the client shows 99 minus what the player already owns. Below 60000 the
	// part moves to the home country lot.
	normalSalesStock = 60000
	// salesFlagNormal is the normal shop. The capture lot uses 'D'.
	salesFlagNormal = 'N'
)

// Record is one row of the buy lineup: the number the client looks a part up
// by plus its price. Stock and flag are constants for the normal shop.
type Record struct {
	PartID uint32
	Price  uint32
}

// Family is the part group the client sorts by. WH and GA are treated as
// weapons even though their stems do not start with WP.
const (
	familyCockpit     = 1
	familyLegs        = 2
	familyDevice      = 3
	familyGenerator   = 4
	familyWeapon      = 5
	familyAssist      = 6
	familyJoint       = 7
	familyWeaponAlias = familyWeapon // WH and GA stems
)

// subtypeCodes maps family -> subtype letters in id order. Index in the slice
// is subtype-1; the client derives the tab from this position, not the stem.
var subtypeCodes = map[uint8][]string{
	familyCockpit:   {"CA", "CB", "CC"},
	familyLegs:      {"TL", "RJ", "ML", "CL", "HL", "WL"},
	familyDevice:    {"MC", "AS", "CT"},
	familyGenerator: {"GA", "GB", "GC"},
	familyWeapon:    {"HC", "GN", "HW", "MT", "SG", "SR", "AR", "MG", "ST", "GL", "CR", "CP", "LM", "CB", "RL", "LR", "MS", "SH"},
	familyAssist:    {"RC", "FS", "SP", "RD", "SD", "AM", "LT", "MC", "JM", "NM"},
	familyJoint:     {"UJ", "RJ", "LJ"},
}

// AssetPartID converts a code stem such as "CK_CA001" into the number the shop
// uses: family<<24 | subtype<<16 | serial. Returns (0, false) for stems the
// catalog cannot express.
func AssetPartID(stem string) (uint32, bool) {
	if len(stem) < 6 || stem[2] != '_' {
		return 0, false
	}
	prefix, subtype := stem[:2], stem[3:5]
	family, ok := familyOf(prefix)
	if !ok {
		return 0, false
	}
	index := -1
	for i, code := range subtypeCodes[family] {
		if code == subtype {
			index = i
			break
		}
	}
	if index < 0 {
		return 0, false
	}
	serial, ok := parseSerial(stem[5:])
	if !ok {
		return 0, false
	}
	return uint32(family)<<24 | uint32(index+1)<<16 | serial, true
}

func familyOf(prefix string) (uint8, bool) {
	switch prefix {
	case "CK":
		return familyCockpit, true
	case "LG":
		return familyLegs, true
	case "CP":
		return familyDevice, true
	case "GE":
		return familyGenerator, true
	case "WP", "WH", "GA":
		// WH guns and GA parts are both treated as weapons.
		return familyWeaponAlias, true
	case "AX":
		return familyAssist, true
	case "WJ":
		return familyJoint, true
	default:
		return 0, false
	}
}

func parseSerial(digits string) (uint32, bool) {
	if digits == "" {
		return 0, false
	}
	var serial uint32
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
		serial = serial*10 + uint32(digits[i]-'0')
		if serial > 0xffff {
			return 0, false
		}
	}
	return serial, true
}

// partByID resolves a part id back to its catalog entry (first match), the way
// the TypeScript implementation falls back from stem to id.
func partByID(id uint32) (Part, bool) {
	for _, p := range Parts {
		if stemID, ok := AssetPartID(p.Stem); ok && stemID == id {
			return p, true
		}
	}
	return Part{}, false
}

// catalogPrice returns the buy price for a stem: the price on its own catalog
// row, else the price of the catalog part with the same id, else 0.
func catalogPrice(stem string) uint32 {
	if id, ok := AssetPartID(stem); ok {
		if p, found := partByID(id); found {
			return p.Price
		}
	}
	return 0
}

// spreadByPrefix interleaves grouped stems one index at a time, so one family
// (e.g. all AX_ rows) cannot fill the lineup ahead of every other family.
func spreadByPrefix(stems []string) []string {
	var groups [][]string
	for _, stem := range stems {
		prefix := stem
		if len(stem) > 5 {
			prefix = stem[:5]
		}
		if n := len(groups); n > 0 && len(groups[n-1][0]) >= 5 && groups[n-1][0][:5] == prefix {
			groups[n-1] = append(groups[n-1], stem)
			continue
		}
		groups = append(groups, []string{stem})
	}
	var spread []string
	for index := 0; ; index++ {
		added := false
		for _, group := range groups {
			if index < len(group) {
				spread = append(spread, group[index])
				added = true
			}
		}
		if !added {
			return spread
		}
	}
}

// catalogStems lists the stems of one faction in catalog order, deduplicated
// by part id. Campaign-only parts are not in the catalog and so never appear.
func catalogStems(faction string) []string {
	var stems []string
	seen := make(map[uint32]struct{})
	for _, p := range Parts {
		id, ok := AssetPartID(p.Stem)
		if p.Faction != faction || !ok {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		stems = append(stems, p.Stem)
	}
	return stems
}

// buyStems is the faction's buy lineup: the hound parts first (so complete-hound
// presets stay purchasable), then the rest of the faction, capped at 160.
func buyStems(faction string) []string {
	var stems []string
	seen := make(map[uint32]struct{})
	add := func(stem string) {
		id, ok := AssetPartID(stem)
		if !ok {
			return
		}
		if _, dup := seen[id]; dup {
			return
		}
		if len(stems) >= maxLineup {
			return
		}
		seen[id] = struct{}{}
		stems = append(stems, stem)
	}
	for _, stem := range houndStems[faction] {
		add(stem)
	}
	for _, stem := range spreadByPrefix(catalogStems(faction)) {
		add(stem)
	}
	return stems
}

// CurrentLineup returns the buy list for a faction ("A", "B" or "C").
// Any other faction yields an empty list.
func CurrentLineup(faction string) []Record {
	if faction != "A" && faction != "B" && faction != "C" {
		return nil
	}
	stems := buyStems(faction)
	records := make([]Record, 0, len(stems))
	for _, stem := range stems {
		id, ok := AssetPartID(stem)
		if !ok {
			continue
		}
		records = append(records, Record{PartID: id, Price: catalogPrice(stem)})
	}
	return records
}

func writeSection(body []byte, section int, records []Record) {
	base := section * sectionLength
	// Section name. The client keeps the name from the first half.
	copy(body[base+1:], "Buy")

	count := len(records)
	if count > maxPerSection {
		count = maxPerSection
	}
	body[base+0x32] = byte(count)

	if section == 0 && count > 0 {
		featured := records
		if len(featured) > 5 {
			featured = featured[:5]
		}
		for i, record := range featured {
			binary.LittleEndian.PutUint32(body[base+0x18+i*4:], record.PartID)
		}
		hit := records[0]
		if hitID, ok := AssetPartID(hitPartStem); ok {
			for _, record := range records {
				if record.PartID == hitID {
					hit = record
					break
				}
			}
		}
		binary.LittleEndian.PutUint32(body[base+0x2c:], hit.PartID)
	}

	cursor := base + recordOffset
	for i := 0; i < count; i++ {
		binary.LittleEndian.PutUint32(body[cursor:], records[i].PartID)
		binary.LittleEndian.PutUint16(body[cursor+4:], normalSalesStock)
		binary.LittleEndian.PutUint16(body[cursor+6:], uint16(records[i].Price&0xffff))
		body[cursor+8] = salesFlagNormal
		cursor += recordSize
	}
}

// BuildResponse assembles the shop reply: the request header, then the first 80
// parts in section 0 and the rest in section 1.
func BuildResponse(request []byte, records []Record) []byte {
	packet := make([]byte, HeaderLength+sectionLength*2)
	if len(request) > HeaderLength {
		request = request[:HeaderLength]
	}
	copy(packet, request)

	body := packet[HeaderLength:]
	first, second := 0, 0
	if len(records) > maxPerSection {
		first = maxPerSection
	}
	if len(records) > first {
		second = len(records) - first
		if second > maxPerSection {
			second = maxPerSection
		}
	}
	writeSection(body, 0, records[:first])
	writeSection(body, 1, records[first:first+second])
	return packet
}
