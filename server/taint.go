package server

import (
	"ChromehoundsStatusServer/logging"
	"os"
	"sort"
	"strings"
)

// Provenance taint: a shared diagnostic that fills a message's reserved / unmapped wire region with
// self-describing markers, so when a tainted byte turns up in client memory (via a debugger read or a
// packet log) its marker names EXACTLY which of our messages produced it and at which offset. It is the
// general form of the one-off WEAPON_FUZZ used to rule the World Tail out as the unidentified-weapon
// source: point it at any server's reserved bytes and read the client to see where they land.
//
// It is a taint, not gameplay: the region is overwritten with markers instead of real data, so run it only
// for RE captures (SERVER_TAINT unset = normal). Applied POST-ENCODE (raw response bytes), never on the
// struct, so multi-byte fields are not re-ordered by the little-endian encoder -- the markers reach the
// wire exactly as written.
//
// Marker layout (one per 4-byte slot at region offset k): {0xFE, tag, k>>8, k&0xFF}.
//   - 0xFE is a sentinel that (almost) never occurs naturally, so markers are greppable.
//   - tag names the source message (TaintTag below).
//   - k is the byte offset within the tainted region -> the exact wire offset that fed a client field.
// TaintDecode recognises both byte orders, since the client deserializer may byte-swap the dword.

// TaintTag identifies a taint source (a server message + its reserved region). Add one per server you want
// to ring out; keep the values stable so old capture dumps stay decodable.
type TaintTag byte

const (
	TaintWorld     TaintTag = 0x01 // World Situation (195): the 332-byte Tail
	TaintWorldArea TaintTag = 0x02 // World Area (196): the unused area records (22..24)
	TaintAreaInfo  TaintTag = 0x03 // Area Info (197): unused map slots (reserved for future wiring)
	TaintMapDetail TaintTag = 0x04 // Map Detail (198): reserved
	TaintStatus    TaintTag = 0x05 // Status (187): reserved
)

var taintNames = map[TaintTag]string{
	TaintWorld:     "world",
	TaintWorldArea: "worldarea",
	TaintAreaInfo:  "areainfo",
	TaintMapDetail: "mapdetail",
	TaintStatus:    "status",
}

// taintEnabled is the set of tags to taint, parsed once from SERVER_TAINT (comma-separated tag names, or
// "all"). WEAPON_FUZZ=1 is kept as a back-compat alias for SERVER_TAINT=world.
var taintEnabled = parseTaintEnv()

func parseTaintEnv() map[TaintTag]bool {
	spec := strings.TrimSpace(os.Getenv("SERVER_TAINT"))
	if spec == "" && os.Getenv("WEAPON_FUZZ") == "1" {
		spec = "world" // back-compat alias
	}
	m := map[TaintTag]bool{}
	if spec == "" {
		return m
	}
	if strings.EqualFold(spec, "all") {
		for t := range taintNames {
			m[t] = true
		}
		return m
	}
	byName := make(map[string]TaintTag, len(taintNames))
	for t, n := range taintNames {
		byName[n] = t
	}
	for _, part := range strings.Split(spec, ",") {
		if t, ok := byName[strings.ToLower(strings.TrimSpace(part))]; ok {
			m[t] = true
		}
	}
	return m
}

func init() {
	// Logged unconditionally, both states: a capture must never be silently confounded by not knowing
	// whether a message carries the provenance taint or real data.
	if len(taintEnabled) == 0 {
		logging.Info.Printf("SERVER_TAINT off -- all messages served with real data.")
		return
	}
	on := make([]string, 0, len(taintEnabled))
	for t := range taintEnabled {
		on = append(on, taintNames[t])
	}
	sort.Strings(on)
	logging.Warn.Printf("SERVER_TAINT ACTIVE for [%s] -- these messages' reserved regions carry provenance "+
		"markers (FE<tag><off16>) instead of real data. Diagnostic only; unset SERVER_TAINT (and WEAPON_FUZZ) "+
		"for normal captures.", strings.Join(on, ","))
}

// TaintActive reports whether tag's region should be tainted this run.
func TaintActive(t TaintTag) bool { return taintEnabled[t] }

// TaintFill writes provenance markers over buf: each 4-byte slot at offset k becomes
// {0xFE, tag, k>>8, k}. A trailing partial slot (<4 bytes) still gets the sentinel and as many offset
// bytes as fit, so even a misaligned copy leaves a recognisable FE.
func TaintFill(buf []byte, t TaintTag) {
	for k := 0; k < len(buf); k += 4 {
		buf[k] = 0xFE
		if k+1 < len(buf) {
			buf[k+1] = byte(t)
		}
		if k+2 < len(buf) {
			buf[k+2] = byte(k >> 8)
		}
		if k+3 < len(buf) {
			buf[k+3] = byte(k)
		}
	}
}

// TaintHit is one decoded marker found in a client-memory / packet dump.
type TaintHit struct {
	Tag       TaintTag // which message the byte came from
	TagName   string
	SrcOffset int  // byte offset within that message's tainted region
	PosInDump int  // where the marker appeared in the scanned data
	Swapped   bool // marker was byte-reversed (client byte-swapped the dword)
}

// TaintDecode scans a byte dump (e.g. a weapon-object read) for provenance markers and reports their
// source message + offset. It matches both {FE,tag,hi,lo} and the byte-swapped {lo,hi,tag,FE}.
func TaintDecode(data []byte) []TaintHit {
	var hits []TaintHit
	// Forward tokenizer: consume 4 bytes on a match so a dense run of markers isn't double-counted by the
	// swapped check overlapping the next normal marker.
	for i := 0; i+4 <= len(data); {
		if data[i] == 0xFE {
			if name, ok := taintNames[TaintTag(data[i+1])]; ok {
				hits = append(hits, TaintHit{
					Tag: TaintTag(data[i+1]), TagName: name,
					SrcOffset: int(data[i+2])<<8 | int(data[i+3]), PosInDump: i,
				})
				i += 4
				continue
			}
		}
		if data[i+3] == 0xFE {
			if name, ok := taintNames[TaintTag(data[i+2])]; ok {
				hits = append(hits, TaintHit{
					Tag: TaintTag(data[i+2]), TagName: name,
					SrcOffset: int(data[i+1])<<8 | int(data[i]), PosInDump: i, Swapped: true,
				})
				i += 4
				continue
			}
		}
		i++
	}
	return hits
}
