package server

import (
	"ChromehoundsStatusServer/constants"
	"ChromehoundsStatusServer/persistence"
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"testing"
)

// squadConfigPacket builds a 1205/1245 request (header + gamertag + teamid + 26-byte settings blob).
func squadConfigPacket(blob []byte) []byte {
	pkt := make([]byte, constants.MinHelloMessageSize+squadConfigBodySize)
	if _, err := binary.Encode(pkt, binary.LittleEndian, CreateHeader(squadXuid, [8]byte{})); err != nil {
		panic(err)
	}
	body := pkt[constants.MinHelloMessageSize:]
	copy(body[0:], "ibac")
	copy(body[16:], "TM0001000000000001")
	copy(body[34:], blob)
	return pkt
}

// blobs captured from the game: a create-squad upload (both sections) and a change-settings upload
// (main section only).
var (
	createConfigBlob = []byte{
		0x00, 0x03, 0x01, 0xa0, 0xa0, 0xa0, 0x6e, 0x73, 0x6e, 0x5f, 0x5a, 0x50, 0x00,
		0x00, 0x00, 0x04, 0x01, 0x01, 0x31, 0x01, 0x3f, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	changeConfigBlob = []byte{
		0x00, 0x01, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x04, 0x03, 0x43, 0x07, 0x20, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
)

func TestParseSquadConfigCreate(t *testing.T) {
	teamID, flags, s, ok := parseSquadConfig(squadConfigPacket(createConfigBlob))
	if !ok {
		t.Fatal("parse failed")
	}
	if teamID != "TM0001000000000001" {
		t.Errorf("teamID = %q", teamID)
	}
	if flags != 0x03 {
		t.Errorf("flags = %#x, want 0x03 (both sections)", flags)
	}
	if s.Stance != 1 || s.Activity != 1 || s.Language != 0x31 || s.Regions != 1 || s.RoleFlags != 0x3f {
		t.Errorf("main settings = %+v", s)
	}
	// Colours are the full 4-RGB palette; the create blob carried four distinct colours.
	if !bytes.Equal(s.Colors, []byte{0xa0, 0xa0, 0xa0, 0x6e, 0x73, 0x6e, 0x5f, 0x5a, 0x50, 0x00, 0x00, 0x00}) {
		t.Errorf("colors = % x", s.Colors)
	}
	if s.Patern != 0x04 {
		t.Errorf("patern = %#x, want 0x04", s.Patern)
	}
}

func TestParseSquadConfigChange(t *testing.T) {
	_, flags, s, ok := parseSquadConfig(squadConfigPacket(changeConfigBlob))
	if !ok {
		t.Fatal("parse failed")
	}
	if flags != 0x01 {
		t.Errorf("flags = %#x, want 0x01 (main only)", flags)
	}
	if s.Stance != 0x04 || s.Activity != 0x03 || s.Language != 0x43 || s.Regions != 0x07 || s.RoleFlags != 0x20 {
		t.Errorf("changed main settings = %+v", s)
	}
}

// TestSquadSettingsMergeLive proves the section-merge: uploading the main section then the colours
// section leaves both persisted (editing roles must not wipe the passcode). Skipped without Mongo.
func TestSquadSettingsMergeLive(t *testing.T) {
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set MONGO_TEST_URI to run the live settings-merge test")
	}
	ctx := context.Background()

	store, err := persistence.Connect(ctx, uri, "combas_test")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer store.Close(ctx)

	repo := NewSquadRepository(store)
	_ = repo.squads.Drop(ctx)
	_ = repo.profiles.Drop(ctx)
	_ = repo.counters.Drop(ctx)
	t.Cleanup(func() {
		_ = repo.squads.Drop(ctx)
		_ = repo.profiles.Drop(ctx)
		_ = repo.counters.Drop(ctx)
	})
	if err := repo.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	p, _ := repo.EnsureProfile(ctx, "000900001AC5EE91", "ibac")
	squad, _ := repo.CreateSquad(ctx, "OpenCombas", "B", p)

	palette := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	// 1) colours section.
	if ok, err := repo.UpdateSquadSettings(ctx, squad.TeamID, 0x02, SquadSettings{Colors: palette, Patern: 4}); err != nil || !ok {
		t.Fatalf("update colours: ok=%v err=%v", ok, err)
	}
	// 2) main section only -> must NOT clear the palette.
	if ok, err := repo.UpdateSquadSettings(ctx, squad.TeamID, 0x01, SquadSettings{Stance: 4, RoleFlags: 0x20}); err != nil || !ok {
		t.Fatalf("update main: ok=%v err=%v", ok, err)
	}

	got, _ := repo.SquadByTeamID(ctx, squad.TeamID)
	if got == nil || got.Settings == nil {
		t.Fatal("settings not persisted")
	}
	if len(got.Settings.Colors) != 12 || got.Settings.Colors[0] != 1 || got.Settings.Patern != 4 {
		t.Errorf("palette clobbered by main-only edit: %+v", got.Settings)
	}
	if got.Settings.Stance != 4 || got.Settings.RoleFlags != 0x20 {
		t.Errorf("main settings not applied: %+v", got.Settings)
	}

	// Unknown team -> not found.
	if ok, _ := repo.UpdateSquadSettings(ctx, "TM9999999999999999", 0x01, SquadSettings{}); ok {
		t.Error("update on unknown team reported existing")
	}
}
