package server

import (
	"ChromehoundsStatusServer/persistence"
	"context"
	"fmt"
	"sort"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Phase 2 persistence for squads. The combas server owns three MongoDB collections holding the
// persistent squad/identity state that squad-reg (1201) and squad-login (1204) previously faked:
//
//   squads         - one doc per squad: name, faction, rank, embedded member roster.
//   combasProfiles - persistent per-player identity (xuid -> assigned UserID, gamertag, current team).
//                    NOTE: deliberately NOT "players" -- Xenia-WebServices owns an ephemeral (TTL 1d)
//                    "players" collection in the same database; this is a separate, persistent concern.
//   combasCounters - monotonic sequences for generating TeamID / UserID numbers.
//
// IDs use the in-game format "TM%04d%012d" / "US%04d%012d" (season 0001 for now), so the first squad
// created gets TM0001000000000001 / US0001000000000001 -- identical to the previous hard-coded values.
// See project_combas_server_protocol memory.

const (
	squadsCollection   = "squads"
	profilesCollection = "combasProfiles"
	countersCollection = "combasCounters"
	statsCollection    = "squadStats"

	teamSeqName = "team"
	userSeqName = "user"
	seasonID    = "0001" // the id-format season used in TeamID/UserID ("TM0001..."); not the war season

	// currentSeason is the war season the stats are bucketed under and the ranking reports (the game's
	// "current season", shown as season 14). Ingest and ranking must agree on this key. TODO: make this
	// config/world-state driven rather than a constant.
	currentSeason = "0014"
)

// CombasProfile is the persistent identity for one player.
type CombasProfile struct {
	XUID     string `bson:"xuid"` // 16-char ASCII-hex XUID (the message-header form)
	UserID   string `bson:"userId"`
	Gamertag string `bson:"gamertag"`
	TeamID   string `bson:"teamId,omitempty"`
}

// SquadMemberRecord is one stored roster entry embedded in a squad (distinct from the wire-format
// SquadMember struct in squadLoginServer.go).
type SquadMemberRecord struct {
	XUID       string `bson:"xuid"`
	UserID     string `bson:"userId"`
	Gamertag   string `bson:"gamertag"`
	Name       string `bson:"name,omitempty"` // in-squad member name (distinct from Live gamertag); must be unique in the squad
	Leader     bool   `bson:"leader"`
	UserNumber int32  `bson:"userNumber"`
	Rank       int32  `bson:"rank"`
}

// SquadSettings is the squad's editable configuration, uploaded via the config message (1205/1245).
// Field labels are inferred from differential capture analysis (create vs change-settings) and the
// create-menu order; the values are stored verbatim so they round-trip regardless of exact labelling.
type SquadSettings struct {
	Stance    int32  `bson:"stance"`    // blob[50] - "Stance"
	Activity  int32  `bson:"activity"`  // blob[51] - "Activity Level"
	Language  int32  `bson:"language"`  // blob[52] - "Language"
	Regions   int32  `bson:"regions"`   // blob[53] - "Connected Regions" (likely a bitmask)
	RoleFlags int32  `bson:"roleFlags"` // blob[54] - 6-bit role bitmask (0x3f = all six on)
	Colors    []byte `bson:"colors,omitempty"` // blob[37..48] - 4 RGB team colours (12 bytes)
	Patern    int32  `bson:"patern"`           // blob[49] - palette/pattern selector (4 in create, 1 single-colour)
}

// Squad is a persistent guild.
type Squad struct {
	TeamID   string              `bson:"teamId"`
	Name     string              `bson:"name"`
	Faction  string              `bson:"faction"` // "A"/"B"/"C"
	Rank     int32               `bson:"rank"`
	Members  []SquadMemberRecord `bson:"members"`
	Settings *SquadSettings      `bson:"settings,omitempty"`
	Emblems  []byte              `bson:"emblems,omitempty"` // 16 emblem layers (192 bytes, wire "S4,C3" format)
}

// UpdateSquadSettings applies an uploaded config to a squad, honouring the section flags: bit0 sets the
// main settings (stance/activity/language/regions/roles), bit1 sets colours/passcode. Only the present
// sections are written, so editing one section never clobbers the other. Returns whether the squad
// existed (false -> "Target Team Not Exist").
func (r *SquadRepository) UpdateSquadSettings(ctx context.Context, teamID string, flags byte, s SquadSettings) (bool, error) {
	set := bson.M{}
	if flags&1 != 0 {
		set["settings.stance"] = s.Stance
		set["settings.activity"] = s.Activity
		set["settings.language"] = s.Language
		set["settings.regions"] = s.Regions
		set["settings.roleFlags"] = s.RoleFlags
	}
	if flags&2 != 0 {
		set["settings.colors"] = s.Colors
		set["settings.patern"] = s.Patern
	}
	if len(set) == 0 {
		// Nothing flagged; still report existence so the client gets "complete".
		n, err := r.squads.CountDocuments(ctx, bson.M{"teamId": teamID})
		return n > 0, err
	}
	res, err := r.squads.UpdateOne(ctx, bson.M{"teamId": teamID}, bson.M{"$set": set})
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

// SquadRepository reads/writes the squad collections on the shared MongoDB.
type SquadRepository struct {
	squads   *mongo.Collection
	profiles *mongo.Collection
	counters *mongo.Collection
	stats    *mongo.Collection
}

func NewSquadRepository(store *persistence.Store) *SquadRepository {
	return &SquadRepository{
		squads:   store.Collection(squadsCollection),
		profiles: store.Collection(profilesCollection),
		counters: store.Collection(countersCollection),
		stats:    store.Collection(statsCollection),
	}
}

// StatBucket holds a running total plus per-season sub-totals (the squad ranking is queryable as Total
// or current Season). BySeason is keyed by season id (e.g. "0001").
type StatBucket struct {
	Total    int32            `bson:"total"`
	BySeason map[string]int32 `bson:"bySeason,omitempty"`
}

// SquadStats is the persistent per-squad leaderboard state, accumulated from battle reports (1254). It
// feeds the squad ranking (1262): Renown, Capture Points, and Renown-Per-Member (derived at read time as
// Renown / live member count).
type SquadStats struct {
	TeamID        string     `bson:"teamId"`
	CapturePoints StatBucket `bson:"capturePoints"`
	Renown        StatBucket `bson:"renown"`
	Battles       struct {
		Won    int32 `bson:"won"`
		Played int32 `bson:"played"`
	} `bson:"battles"`
}

// isRealTeam reports whether teamID is a real squad (vs an AI/CPU placeholder like "BBB9999..."). Real
// squad ids are the assigned "TM" + digits form.
func isRealTeam(teamID string) bool { return strings.HasPrefix(teamID, "TM") }

// CreditBattle records one battle's result into squad stats: the winning squad gains capture points and
// renown (into both the running total and the season bucket) plus a win; both squads get a played count.
// AI/CPU sides (non-"TM" ids) are skipped. Upserts, so a squad's first battle creates its stats doc.
func (r *SquadRepository) CreditBattle(ctx context.Context, winnerTeam, loserTeam string, capturePoints, renown int32, season string) error {
	if isRealTeam(winnerTeam) {
		inc := bson.M{
			"capturePoints.total":              capturePoints,
			"capturePoints.bySeason." + season: capturePoints,
			"renown.total":                     renown,
			"renown.bySeason." + season:        renown,
			"battles.won":                      int32(1),
			"battles.played":                   int32(1),
		}
		if _, err := r.stats.UpdateOne(ctx, bson.M{"teamId": winnerTeam}, bson.M{"$inc": inc}, options.UpdateOne().SetUpsert(true)); err != nil {
			return err
		}
	}
	if isRealTeam(loserTeam) && loserTeam != winnerTeam {
		if _, err := r.stats.UpdateOne(ctx, bson.M{"teamId": loserTeam}, bson.M{"$inc": bson.M{"battles.played": int32(1)}}, options.UpdateOne().SetUpsert(true)); err != nil {
			return err
		}
	}
	return nil
}

// RankEntry is one ranked squad for the leaderboard (1262).
type RankEntry struct {
	TeamID string
	Name   string
	Value  int32
}

// RankSquads returns every squad ranked descending by the requested stat. kbn: 1=Renown,
// 2=Capture-Points, 3=Renown-Per-Member. useSeason selects the season bucket vs the running total.
// Squads with no accumulated stats yet rank with 0. Renown-Per-Member divides by the live roster size.
func (r *SquadRepository) RankSquads(ctx context.Context, kbn int, useSeason bool, season string) ([]RankEntry, error) {
	var squads []Squad
	if cur, err := r.squads.Find(ctx, bson.M{}); err != nil {
		return nil, err
	} else if err := cur.All(ctx, &squads); err != nil {
		return nil, err
	}

	var stats []SquadStats
	if cur, err := r.stats.Find(ctx, bson.M{}); err != nil {
		return nil, err
	} else if err := cur.All(ctx, &stats); err != nil {
		return nil, err
	}
	byTeam := make(map[string]SquadStats, len(stats))
	for _, s := range stats {
		byTeam[s.TeamID] = s
	}

	bucket := func(b StatBucket) int32 {
		if useSeason {
			return b.BySeason[season]
		}
		return b.Total
	}
	entries := make([]RankEntry, 0, len(squads))
	for _, sq := range squads {
		st := byTeam[sq.TeamID]
		renown := bucket(st.Renown)
		var val int32
		switch kbn {
		case 2:
			val = bucket(st.CapturePoints)
		case 3:
			n := int32(len(sq.Members))
			if n < 1 {
				n = 1
			}
			val = renown / n
		default: // 1 = Renown
			val = renown
		}
		entries = append(entries, RankEntry{TeamID: sq.TeamID, Name: sq.Name, Value: val})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Value > entries[j].Value })
	return entries, nil
}

// SquadStatsByTeamID returns a squad's accumulated stats, or (nil, nil) if it has none yet.
func (r *SquadRepository) SquadStatsByTeamID(ctx context.Context, teamID string) (*SquadStats, error) {
	var st SquadStats
	err := r.stats.FindOne(ctx, bson.M{"teamId": teamID}).Decode(&st)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

func formatTeamID(seq int64) string { return fmt.Sprintf("TM%s%012d", seasonID, seq) }
func formatUserID(seq int64) string { return fmt.Sprintf("US%s%012d", seasonID, seq) }

// EnsureSchema creates the unique indexes the squad model relies on.
func (r *SquadRepository) EnsureSchema(ctx context.Context) error {
	if _, err := r.squads.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "teamId", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "name", Value: 1}}, Options: options.Index().SetUnique(true)},
	}); err != nil {
		return err
	}
	if _, err := r.profiles.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "xuid", Value: 1}},
		Options: options.Index().SetUnique(true),
	}); err != nil {
		return err
	}
	_, err := r.stats.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "teamId", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	return err
}

// nextSeq atomically increments and returns the named counter (starts at 1).
func (r *SquadRepository) nextSeq(ctx context.Context, name string) (int64, error) {
	var res struct {
		Seq int64 `bson:"seq"`
	}
	err := r.counters.FindOneAndUpdate(ctx,
		bson.M{"_id": name},
		bson.M{"$inc": bson.M{"seq": int64(1)}},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	).Decode(&res)
	return res.Seq, err
}

// EnsureProfile returns the player's persistent profile, creating it (with a freshly assigned UserID)
// on first sight.
func (r *SquadRepository) EnsureProfile(ctx context.Context, xuid, gamertag string) (CombasProfile, error) {
	var p CombasProfile
	err := r.profiles.FindOne(ctx, bson.M{"xuid": xuid}).Decode(&p)
	if err == nil {
		return p, nil
	}
	if err != mongo.ErrNoDocuments {
		return CombasProfile{}, err
	}

	seq, err := r.nextSeq(ctx, userSeqName)
	if err != nil {
		return CombasProfile{}, err
	}
	p = CombasProfile{XUID: xuid, UserID: formatUserID(seq), Gamertag: gamertag}
	if _, err := r.profiles.InsertOne(ctx, p); err != nil {
		return CombasProfile{}, err
	}
	return p, nil
}

// RemoveMember withdraws a member (by user id) from a squad, returning the status byte the client
// expects (parser sub_823BDB88): '1' Delete Complete, '2' Leader Can't Delete, '3' Target Data None.
// A leader may only leave when they are the last member (the squad then disbands); a leader abandoning
// remaining members is rejected with '2'.
func (r *SquadRepository) RemoveMember(ctx context.Context, teamID, userID string) (byte, error) {
	sq, err := r.SquadByTeamID(ctx, teamID)
	if err != nil {
		return 0, err
	}
	if sq == nil {
		return '3', nil // Target Data None
	}

	idx := -1
	for i, m := range sq.Members {
		if m.UserID == userID {
			idx = i
			break
		}
	}
	if idx == -1 {
		return '3', nil
	}
	if sq.Members[idx].Leader && len(sq.Members) > 1 {
		return '2', nil // Leader Can't Delete (would orphan the remaining members)
	}

	memberXUID := sq.Members[idx].XUID
	if len(sq.Members) <= 1 {
		// Last member leaving -> disband the squad entirely.
		if _, err := r.squads.DeleteOne(ctx, bson.M{"teamId": teamID}); err != nil {
			return 0, err
		}
	} else {
		if _, err := r.squads.UpdateOne(ctx,
			bson.M{"teamId": teamID},
			bson.M{"$pull": bson.M{"members": bson.M{"userId": userID}}},
		); err != nil {
			return 0, err
		}
	}

	// Unlink the departing member's persistent profile from the team.
	if memberXUID != "" {
		_, _ = r.profiles.UpdateOne(ctx, bson.M{"xuid": memberXUID}, bson.M{"$set": bson.M{"teamId": ""}})
	}
	return '1', nil // Delete Complete
}

// squadMemberCap is the maximum roster size (the login wire blob holds 20 member records).
const squadMemberCap = 20

// firstFreeUserNumber returns the lowest hound number (0..99) not already held by a member.
func firstFreeUserNumber(members []SquadMemberRecord) int32 {
	used := make(map[int32]bool, len(members))
	for _, m := range members {
		used[m.UserNumber] = true
	}
	for n := int32(0); n <= 99; n++ {
		if !used[n] {
			return n
		}
	}
	return 0
}

// AddMember commits a joining player to a squad (the code-182 join-commit sent by the squad host).
// It resolves/mints the joiner's persistent profile (keyed by their XUID), appends them to the roster and
// links their profile to the team. Returns the status byte the client expects (parser sub_823BDAA0) plus
// the joiner's assigned User ID on success:
//
//	'1' success (+ userID)            '2' Member Number Over Error (squad full)
//	'3' User Name Unique Error        '0' unknown (target squad does not exist)
//
// Idempotent: a player who is already a member gets their existing User ID back with '1'.
//
// The '3' "User Name Unique Error" check is against the in-squad NAME (not the Live gamertag): gamertags
// are globally unique in real play, so keying uniqueness on them would wrongly reject legitimate joins
// whenever two consoles share a gamertag (as in local testing).
func (r *SquadRepository) AddMember(ctx context.Context, teamID, xuid, gamertag, name string, rank int32) (byte, string, error) {
	sq, err := r.SquadByTeamID(ctx, teamID)
	if err != nil {
		return 0, "", err
	}
	if sq == nil {
		return '0', "", nil // target squad does not exist -> "Unknown Error"
	}

	for _, m := range sq.Members {
		if m.XUID == xuid {
			return '1', m.UserID, nil // already a member -> return existing id (idempotent)
		}
		if name != "" && m.Name == name {
			return '3', "", nil // User Name Unique Error (in-squad name already taken)
		}
	}
	if len(sq.Members) >= squadMemberCap {
		return '2', "", nil // Member Number Over Error (squad full)
	}

	profile, err := r.EnsureProfile(ctx, xuid, gamertag)
	if err != nil {
		return 0, "", err
	}

	member := SquadMemberRecord{
		XUID:       xuid,
		UserID:     profile.UserID,
		Gamertag:   gamertag,
		Name:       name,
		Leader:     false,
		UserNumber: firstFreeUserNumber(sq.Members),
		Rank:       rank,
	}
	if _, err := r.squads.UpdateOne(ctx,
		bson.M{"teamId": teamID},
		bson.M{"$push": bson.M{"members": member}},
	); err != nil {
		return 0, "", err
	}
	if _, err := r.profiles.UpdateOne(ctx,
		bson.M{"xuid": xuid},
		bson.M{"$set": bson.M{"teamId": teamID}},
	); err != nil {
		return 0, "", err
	}
	return '1', profile.UserID, nil
}

// SetMemberNumber assigns a member's "hound number" (0..99, unique within the squad), returning the
// status byte the client expects (parser sub_823BDA28): '1' Member Config Complete, '2' Already Been
// Used By Other Users, '3' Target Member Not Exist.
func (r *SquadRepository) SetMemberNumber(ctx context.Context, teamID, userID string, number byte) (byte, error) {
	sq, err := r.SquadByTeamID(ctx, teamID)
	if err != nil {
		return 0, err
	}
	if sq == nil {
		return '3', nil // Target Member Not Exist
	}

	memberFound := false
	for _, m := range sq.Members {
		if m.UserID == userID {
			memberFound = true
			continue
		}
		if m.UserNumber == int32(number) {
			return '2', nil // Already Been Used By Other Users
		}
	}
	if !memberFound {
		return '3', nil
	}

	if _, err := r.squads.UpdateOne(ctx,
		bson.M{"teamId": teamID, "members.userId": userID},
		bson.M{"$set": bson.M{"members.$.userNumber": int32(number)}},
	); err != nil {
		return 0, err
	}
	return '1', nil // Member Config Complete
}

// SetEmblems stores the raw 192-byte emblem blob on a squad, returning whether the squad existed.
func (r *SquadRepository) SetEmblems(ctx context.Context, teamID string, emblems []byte) (bool, error) {
	res, err := r.squads.UpdateOne(ctx,
		bson.M{"teamId": teamID},
		bson.M{"$set": bson.M{"emblems": emblems}},
	)
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

// SquadByTeamID returns the squad with the given team id, or (nil, nil) if none exists.
func (r *SquadRepository) SquadByTeamID(ctx context.Context, teamID string) (*Squad, error) {
	return r.findSquad(ctx, bson.M{"teamId": teamID})
}

// SquadByName returns the squad with the given name, or (nil, nil) if none exists.
func (r *SquadRepository) SquadByName(ctx context.Context, name string) (*Squad, error) {
	return r.findSquad(ctx, bson.M{"name": name})
}

func (r *SquadRepository) findSquad(ctx context.Context, filter bson.M) (*Squad, error) {
	var sq Squad
	err := r.squads.FindOne(ctx, filter).Decode(&sq)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sq, nil
}

// CreateSquad assigns a team id, inserts the squad with the leader as its sole member, and links the
// leader's profile to the new team.
func (r *SquadRepository) CreateSquad(ctx context.Context, name, faction string, leader CombasProfile) (*Squad, error) {
	seq, err := r.nextSeq(ctx, teamSeqName)
	if err != nil {
		return nil, err
	}
	squad := &Squad{
		TeamID:  formatTeamID(seq),
		Name:    name,
		Faction: faction,
		Rank:    1,
		Members: []SquadMemberRecord{{
			XUID:       leader.XUID,
			UserID:     leader.UserID,
			Gamertag:   leader.Gamertag,
			Leader:     true,
			UserNumber: 1,
			Rank:       1,
		}},
	}
	if _, err := r.squads.InsertOne(ctx, squad); err != nil {
		return nil, err
	}
	if _, err := r.profiles.UpdateOne(ctx,
		bson.M{"xuid": leader.XUID},
		bson.M{"$set": bson.M{"teamId": squad.TeamID}},
	); err != nil {
		return nil, err
	}
	return squad, nil
}
