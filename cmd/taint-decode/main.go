// Command taint-decode reads a hex dump (a debugger memory read, or bytes copied from a packet log) and
// reports which SERVER_TAINT provenance markers it contains -- i.e. which of our messages, at which byte
// offset, produced each tainted value that reached the client.
//
// Pair it with SERVER_TAINT (see server/taint.go): run the server with a message tainted, read the client
// structure you're trying to source (e.g. the weapon-state object), paste the hex here, and every marker
// names its exact wire origin. Provenance a hand-derived offset can't fake.
//
//	# from a debugger dump:
//	go run ./cmd/taint-decode 8205c168000000000000000200000000fe0100000fe010004...
//	# or from stdin / a file:
//	xxd -p dump.bin | go run ./cmd/taint-decode
//
// The input may contain spaces, newlines, 0x prefixes and "| addr:" noise; only hex digit pairs are read.
package main

import (
	"ChromehoundsStatusServer/server"
	"bufio"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var hexPair = regexp.MustCompile(`[0-9a-fA-F]{2}`)

func main() {
	raw := strings.Join(os.Args[1:], " ")
	if strings.TrimSpace(raw) == "" {
		// no args: read stdin
		var b strings.Builder
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
		for sc.Scan() {
			b.WriteString(sc.Text())
			b.WriteByte(' ')
		}
		raw = b.String()
	}
	if strings.TrimSpace(raw) == "" {
		fmt.Fprintln(os.Stderr, "usage: taint-decode <hex...>   (or pipe hex on stdin)")
		os.Exit(2)
	}

	// Keep only hex digit pairs so pasted dumps with addresses/pipes/0x still work.
	clean := strings.Join(hexPair.FindAllString(strings.ReplaceAll(raw, "0x", ""), -1), "")
	if len(clean)%2 == 1 {
		clean = clean[:len(clean)-1] // drop a dangling nibble
	}
	data, err := hex.DecodeString(clean)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad hex: %v\n", err)
		os.Exit(2)
	}

	hits := server.TaintDecode(data)
	fmt.Printf("scanned %d bytes, %d marker(s) found\n", len(data), len(hits))
	if len(hits) == 0 {
		fmt.Println("  (no provenance markers -- this data is NOT fed by any tainted message)")
		return
	}
	fmt.Printf("  %-6s  %-10s  %-12s  %-8s  %s\n", "dumpAt", "source", "srcOffset", "swapped", "note")
	for _, h := range hits {
		note := ""
		if h.Tag == server.TaintWorld {
			// World taint region is the Tail, which begins at World body offset 208.
			note = fmt.Sprintf("World body +%d", 208+h.SrcOffset)
		}
		fmt.Printf("  0x%04x  %-10s  +%-11d  %-8t  %s\n", h.PosInDump, h.TagName, h.SrcOffset, h.Swapped, note)
	}
}
