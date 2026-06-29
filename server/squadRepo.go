package server

import (
	"ChromehoundsStatusServer/persistence"
	"context"
	"fmt"

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

	teamSeqName = "team"
	userSeqName = "user"
	seasonID    = "0001"
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
	Colors    []byte `bson:"colors,omitempty"`   // blob[37..39]
	Passcode  string `bson:"passcode,omitempty"` // blob[40..48]
}

// Squad is a persistent guild.
type Squad struct {
	TeamID   string              `bson:"teamId"`
	Name     string              `bson:"name"`
	Faction  string              `bson:"faction"` // "A"/"B"/"C"
	Rank     int32               `bson:"rank"`
	Members  []SquadMemberRecord `bson:"members"`
	Settings *SquadSettings      `bson:"settings,omitempty"`
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
		set["settings.passcode"] = s.Passcode
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
}

func NewSquadRepository(store *persistence.Store) *SquadRepository {
	return &SquadRepository{
		squads:   store.Collection(squadsCollection),
		profiles: store.Collection(profilesCollection),
		counters: store.Collection(countersCollection),
	}
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
	_, err := r.profiles.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "xuid", Value: 1}},
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
