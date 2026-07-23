package server

import (
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/persistence"
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestRenownContributionFunctional is an end-to-end functional test of the per-player renown ledger against
// a RUNNING server: it seeds a squad, fires a real battle-report UDP packet (crediting three named pilots),
// checks the ledger + squad renown, then fires a withdraw packet for ONE pilot and checks that only that
// member's own contribution was debited. Guarded -- set both to run:
//
//	COMBAS_SERVER_ADDR=<host> MONGO_URI=<uri> [MONGO_DATABASE=test] \
//	  [COMBAS_REPORT_PORT=1214] [COMBAS_WITHDRAW_PORT=1203] \
//	  go test ./server/ -run TestRenownContributionFunctional -v
//
// Uses synthetic ids only. Non-existent battlefield (area 88) so it moves no war state, and a real-format
// (TM..) loser so no CPU renown scaling skews the arithmetic. Cleans up the seeded squad + stats.
func TestRenownContributionFunctional(t *testing.T) {
	addr := os.Getenv("COMBAS_SERVER_ADDR")
	uri := os.Getenv("MONGO_URI")
	if addr == "" || uri == "" {
		t.Skip("set COMBAS_SERVER_ADDR and MONGO_URI to run the functional test")
	}
	db := os.Getenv("MONGO_DATABASE")
	if db == "" {
		db = "test"
	}
	reportPort := envPort("COMBAS_REPORT_PORT", 1214)
	withdrawPort := envPort("COMBAS_WITHDRAW_PORT", 1203)

	const (
		teamID = "TM0001000000009001"
		merit  = byte(90) // 90 / 3 pilots = 30 each
	)
	pilots := []string{"US0001000000009001", "US0001000000009002", "US0001000000009003"}
	const share = int32(30)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	store, err := persistence.Connect(ctx, uri, db)
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()
	repo := NewSquadRepository(store)

	cleanup := func() {
		_, _ = repo.squads.DeleteOne(context.Background(), bson.M{"teamId": teamID})
		_, _ = repo.stats.DeleteOne(context.Background(), bson.M{"teamId": teamID})
	}
	cleanup()
	defer cleanup()

	// Seed the squad with three members, all starting at 0 contribution, faction A (so it wins as nation A).
	members := make([]SquadMemberRecord, len(pilots))
	for i, uid := range pilots {
		members[i] = SquadMemberRecord{
			XUID:       fmt.Sprintf("FUNCXUID%08d", i),
			UserID:     uid,
			Gamertag:   fmt.Sprintf("FUNCPILOT%d", i),
			Name:       fmt.Sprintf("Pilot%d", i),
			Leader:     i == 0,
			UserNumber: int32(i),
		}
	}
	if _, err := repo.squads.InsertOne(ctx, Squad{
		TeamID: teamID, Name: "FUNCTEST_SQUAD", Faction: "A", Members: members,
	}); err != nil {
		t.Fatalf("seed squad: %v", err)
	}

	// --- Battle report: credit the three pilots ---
	t.Logf("firing battle report -> %s:%d (winner %s, merit %d, 3 pilots)", addr, reportPort, teamID, merit)
	sendUDPExpectAck(t, addr, reportPort, buildFuncReport(teamID, pilots, merit))

	waitFor(t, "each pilot credited %d and squad renown %d", func() (bool, string) {
		sq := mustSquad(t, ctx, repo, teamID)
		st := squadRenownTotal(ctx, repo, teamID)
		for _, m := range sq.Members {
			if m.RenownContribution != share {
				return false, fmt.Sprintf("member %s contribution=%d (want %d), renown.total=%d", m.UserID, m.RenownContribution, share, st)
			}
		}
		if st != int32(merit) {
			return false, fmt.Sprintf("renown.total=%d, want %d", st, merit)
		}
		return true, fmt.Sprintf("all 3 pilots @ %d, renown.total=%d", share, st)
	}, share, merit)

	// --- Withdraw one pilot (index 1, a non-leader): only their own 30 comes out ---
	leaver := pilots[1]
	t.Logf("firing withdraw -> %s:%d (member %s)", addr, withdrawPort, leaver)
	sendUDPExpectAck(t, addr, withdrawPort, buildFuncWithdraw(teamID, leaver, members[1].XUID))

	waitFor(t, "leaver removed, renown debited by exactly their %d (-> %d), others unchanged", func() (bool, string) {
		sq := mustSquad(t, ctx, repo, teamID)
		for _, m := range sq.Members {
			if m.UserID == leaver {
				return false, "leaver still on the roster"
			}
			if m.RenownContribution != share {
				return false, fmt.Sprintf("remaining member %s contribution=%d, want %d (unchanged)", m.UserID, m.RenownContribution, share)
			}
		}
		if len(sq.Members) != len(pilots)-1 {
			return false, fmt.Sprintf("roster size %d, want %d", len(sq.Members), len(pilots)-1)
		}
		st := squadRenownTotal(ctx, repo, teamID)
		if want := int32(merit) - share; st != want {
			return false, fmt.Sprintf("renown.total=%d, want %d (only the leaver's %d removed)", st, want, share)
		}
		return true, fmt.Sprintf("leaver gone, renown.total=%d (-%d), other two still @ %d", int32(merit)-share, share, share)
	}, share, int32(merit)-share)

	t.Log("FUNCTIONAL TEST PASSED: contribution credited per pilot, withdraw debited only the leaver's share")
}

func envPort(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// buildFuncReport crafts a 565-byte report body: our squad (nation A / block-1) wins with `merit`, three
// named pilots in its block, an inert non-existent battlefield, and a real-format loser (no CPU scaling).
func buildFuncReport(teamID string, pilots []string, merit byte) []byte {
	pkt := make([]byte, constants.MinHelloMessageSize+battleReportBodySize)
	copy(pkt[0:], []byte{'C', 'H', 0, 0})
	copy(pkt[4:], "000900000000FUNC")
	b := pkt[constants.MinHelloMessageSize:]
	b[0x23], b[0x24] = 88, 1 // non-existent area/map -> no war-state change
	b[0x25] = 'A'
	copy(b[0x26:], teamID)
	for i, p := range pilots {
		copy(b[reportBlock1Users+i*reportUserIDBytes:], p)
	}
	b[0x11C] = merit // team-0 (block-1) merit
	b[0x12C] = 'B'
	copy(b[0x12D:], "TM0001000000009002") // real-format loser -> no CPU scaling
	b[0x233] = 'A'                        // winner nation
	b[0x234] = 100                        // occupation delta
	return pkt
}

func buildFuncWithdraw(teamID, userID, xuid string) []byte {
	body := "FUNCGT," + teamID + "," + userID
	pkt := make([]byte, constants.MinHelloMessageSize+len(body)+1)
	copy(pkt[0:], []byte{'C', 'H', 0, 0})
	copy(pkt[4:20], xuid) // 16-byte header XUID -- the reliable leaver key RemoveMember resolves first
	copy(pkt[constants.MinHelloMessageSize:], body)
	return pkt
}

// sendUDPExpectAck sends the packet and waits for the server's ack -- receiving it confirms the packet
// reached the running server and was processed (both servers reply).
func sendUDPExpectAck(t *testing.T, addr string, port int, pkt []byte) {
	target := net.JoinHostPort(addr, strconv.Itoa(port))
	conn, err := net.Dial("udp", target)
	if err != nil {
		t.Fatalf("dial %s: %v", target, err)
	}
	defer conn.Close()
	if _, err := conn.Write(pkt); err != nil {
		t.Fatalf("write to %s: %v", target, err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("no ack from %s (server unreachable or rejected the packet): %v", target, err)
	}
	t.Logf("  <- ack %d bytes", n)
}

func mustSquad(t *testing.T, ctx context.Context, repo *SquadRepository, teamID string) Squad {
	var sq Squad
	if err := repo.squads.FindOne(ctx, bson.M{"teamId": teamID}).Decode(&sq); err != nil {
		t.Fatalf("read squad: %v", err)
	}
	return sq
}

func squadRenownTotal(ctx context.Context, repo *SquadRepository, teamID string) int32 {
	var st SquadStats
	if err := repo.stats.FindOne(ctx, bson.M{"teamId": teamID}).Decode(&st); err != nil {
		return 0
	}
	return st.Renown.Total
}

// waitFor polls the check for up to ~5s (the server processes asynchronously), failing with the last reason.
func waitFor(t *testing.T, what string, check func() (bool, string), args ...interface{}) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		ok, msg := check()
		if ok {
			t.Logf("  OK: "+what+" -- %s", append(append([]interface{}{}, args...), msg)...)
			return
		}
		last = msg
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: "+what+" -- last: %s", append(append([]interface{}{}, args...), last)...)
}

// TestRenownContributionUniqueShareFunctional proves the debit is each member's OWN accumulated contribution,
// not a flat share. Three reports with different rosters build lopsided totals -- leader small, one non-leader
// heavy -- then the heavy non-leader withdraws and must take out exactly their own total, leaving the others
// untouched. Same guards/env as TestRenownContributionFunctional.
func TestRenownContributionUniqueShareFunctional(t *testing.T) {
	addr := os.Getenv("COMBAS_SERVER_ADDR")
	uri := os.Getenv("MONGO_URI")
	if addr == "" || uri == "" {
		t.Skip("set COMBAS_SERVER_ADDR and MONGO_URI to run the functional test")
	}
	db := os.Getenv("MONGO_DATABASE")
	if db == "" {
		db = "test"
	}
	reportPort := envPort("COMBAS_REPORT_PORT", 1214)
	withdrawPort := envPort("COMBAS_WITHDRAW_PORT", 1203)

	const teamID = "TM0001000000009003"
	pilots := []string{"US0001000000009011", "US0001000000009012", "US0001000000009013"} // [0]=leader
	// Reports (roster -> merit): builds distinct totals well clear of any flat share.
	//   all three @ 30  -> +10 each
	//   pilot[1] alone @ 100 -> +100
	//   pilot[1],pilot[2] @ 40 -> +20 each
	// => contributions {9011:10, 9012:130, 9013:30}, renown 170 (a flat share would be 170/3 = 56).
	want := map[string]int32{pilots[0]: 10, pilots[1]: 130, pilots[2]: 30}
	const totalRenown = int32(170)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	store, err := persistence.Connect(ctx, uri, db)
	if err != nil {
		t.Fatalf("mongo connect: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()
	repo := NewSquadRepository(store)

	cleanup := func() {
		_, _ = repo.squads.DeleteOne(context.Background(), bson.M{"teamId": teamID})
		_, _ = repo.stats.DeleteOne(context.Background(), bson.M{"teamId": teamID})
	}
	cleanup()
	defer cleanup()

	members := make([]SquadMemberRecord, len(pilots))
	for i, uid := range pilots {
		members[i] = SquadMemberRecord{
			XUID:     fmt.Sprintf("UNIQXUID%08d", i),
			UserID:   uid,
			Gamertag: fmt.Sprintf("UNIQPILOT%d", i),
			Name:     fmt.Sprintf("UPilot%d", i),
			Leader:   i == 0,
		}
	}
	if _, err := repo.squads.InsertOne(ctx, Squad{TeamID: teamID, Name: "UNIQ_SQUAD", Faction: "A", Members: members}); err != nil {
		t.Fatalf("seed squad: %v", err)
	}

	t.Logf("firing 3 reports (all@30, %s alone@100, %s+%s @40) -> %s:%d", pilots[1], pilots[1], pilots[2], addr, reportPort)
	sendUDPExpectAck(t, addr, reportPort, buildFuncReport(teamID, pilots, 30))
	sendUDPExpectAck(t, addr, reportPort, buildFuncReport(teamID, pilots[1:2], 100))
	sendUDPExpectAck(t, addr, reportPort, buildFuncReport(teamID, pilots[1:3], 40))

	waitFor(t, "distinct per-pilot totals 9011=10 9012=130 9013=30, renown 170", func() (bool, string) {
		sq := mustSquad(t, ctx, repo, teamID)
		for _, m := range sq.Members {
			if m.RenownContribution != want[m.UserID] {
				return false, fmt.Sprintf("member %s contribution=%d, want %d", m.UserID, m.RenownContribution, want[m.UserID])
			}
		}
		if st := squadRenownTotal(ctx, repo, teamID); st != totalRenown {
			return false, fmt.Sprintf("renown.total=%d, want %d", st, totalRenown)
		}
		return true, "9011=10, 9012=130, 9013=30, renown=170"
	})

	// Withdraw the heavy NON-leader contributor. Their own 130 must come out -- NOT a flat 170/3=56.
	leaver := pilots[1]
	t.Logf("firing withdraw of the heavy contributor %s (own total 130) -> %s:%d", leaver, addr, withdrawPort)
	sendUDPExpectAck(t, addr, withdrawPort, buildFuncWithdraw(teamID, leaver, members[1].XUID))

	waitFor(t, "leaver's UNIQUE 130 debited (renown 170->40), others intact", func() (bool, string) {
		sq := mustSquad(t, ctx, repo, teamID)
		got := map[string]int32{}
		for _, m := range sq.Members {
			if m.UserID == leaver {
				return false, "leaver still present"
			}
			got[m.UserID] = m.RenownContribution
		}
		if got[pilots[0]] != 10 || got[pilots[2]] != 30 {
			return false, fmt.Sprintf("remaining %v, want 9011=10 9013=30 (unchanged)", got)
		}
		if st := squadRenownTotal(ctx, repo, teamID); st != totalRenown-130 {
			return false, fmt.Sprintf("renown.total=%d, want %d (leaver's UNIQUE 130 removed, not a flat 56)", st, totalRenown-130)
		}
		return true, "leaver's own 130 removed (170->40); 9011=10 & 9013=30 untouched -- unique share, not flat 56"
	})

	t.Log("UNIQUE-SHARE TEST PASSED: withdraw debited the heavy contributor's own 130, not a flat 170/3")
}
