package server

import (
	"ChromehoundsStatusServer/logging"
	"ChromehoundsStatusServer/persistence"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

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
	historyCollection  = "squadHistory"
	// playersCollection is the Xenia-WebServices logins collection (same shared DB): the authoritative
	// xuid->gamertag, set from each console's own login. Read-only from here.
	playersCollection = "players"

	teamSeqName = "team"
	userSeqName = "user"
	seasonID    = "0001" // the id-format season used in TeamID/UserID ("TM0001..."); not the war season
)

// currentSeason is the war season the stats are bucketed under and the ranking reports. Ingest and ranking
// must agree on this key. It is DB-backed now (set by the reset tool's -season and loaded at startup via
// ApplySeasonNumber); the default keeps the historical "0014" bucket. See season.go.
var currentSeason = SeasonKey(defaultSeasonNumber)

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
	// RenownContribution is how much of the squad's renown THIS member actually earned: each battle report
	// names the pilots who fought (see BattleResult.WinnerUserIDs), and that battle's renown is split among
	// them and added here. On withdraw the member takes exactly this back out of the squad's renown, so a
	// member who never fought contributes and forfeits 0 -- unlike the old flat Renown/N share.
	RenownContribution int32 `bson:"renownContribution,omitempty"`
}

// SquadSettings is the squad's editable configuration, uploaded via the config message (1205/1245).
// Field labels are inferred from differential capture analysis (create vs change-settings) and the
// create-menu order; the values are stored verbatim so they round-trip regardless of exact labelling.
type SquadSettings struct {
	Stance    int32  `bson:"stance"`           // blob[50] - "Stance"
	Activity  int32  `bson:"activity"`         // blob[51] - "Activity Level"
	Language  int32  `bson:"language"`         // blob[52] - "Language"
	Regions   int32  `bson:"regions"`          // blob[53] - "Connected Regions" (likely a bitmask)
	RoleFlags int32  `bson:"roleFlags"`        // blob[54] - 6-bit role bitmask (0x3f = all six on)
	Colors    []byte `bson:"colors,omitempty"` // blob[37..48] - 4 RGB team colours (12 bytes)
	Patern    int32  `bson:"patern"`           // blob[49] - palette/pattern selector (4 in create, 1 single-colour)
}

// Squad is a persistent guild.
type Squad struct {
	TeamID  string `bson:"teamId"`
	Name    string `bson:"name"`
	Faction string `bson:"faction"` // "A"/"B"/"C"
	Rank    int32  `bson:"rank"`
	// Grade is the squad grade index (squadGradeMin..squadGradeMax), served as team-header off 19 and
	// rendered by the client as FMG string 5700+Grade. Squads created before this field existed decode as
	// 0, which the login path floors to squadGradeMin -- see clampSquadGrade. Derived from lifetime renown
	// on a log ladder (see squadGrade.go); this stored copy is what RefreshSquadGrade persists so grade
	// changes can raise a history event.
	Grade    int32               `bson:"grade,omitempty"`
	Members  []SquadMemberRecord `bson:"members"`
	Settings *SquadSettings      `bson:"settings,omitempty"`
	Emblems  []byte              `bson:"emblems,omitempty"` // 16 emblem layers (192 bytes, wire "S4,C3" format)
	// UpdateSeq is a per-squad monotonic serial bumped on every roster/config mutation (bumpSeq). It is
	// serialized into the login response's TeamInfoCount (team-header off 88): the client re-accepts +
	// peer-rebroadcasts the roster only when this exceeds the value it cached, so a genuine change is
	// "VALID" (propagated) while a stable re-login is "OLD" (not re-processed). Starts at 1 on create.
	UpdateSeq int32 `bson:"updateSeq"`
}

// bumpSeq is the $inc payload every squad mutation adds so UpdateSeq advances on any roster/config change
// (member add/remove, settings, member number, emblem, gamertag). Login serves UpdateSeq as TeamInfoCount.
var bumpSeq = bson.M{"updateSeq": 1}

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
	res, err := r.squads.UpdateOne(ctx, bson.M{"teamId": teamID}, bson.M{"$set": set, "$inc": bumpSeq})
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
	history  *mongo.Collection
	players  *mongo.Collection // webservices logins: authoritative xuid->gamertag (read-only)
}

func NewSquadRepository(store *persistence.Store) *SquadRepository {
	return &SquadRepository{
		squads:   store.Collection(squadsCollection),
		profiles: store.Collection(profilesCollection),
		counters: store.Collection(countersCollection),
		stats:    store.Collection(statsCollection),
		history:  store.Collection(historyCollection),
		players:  store.Collection(playersCollection),
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
		// The loser FORFEITS the same capture points the winner took. This mirrors the world layer, where a
		// mission's OccDelta is added to the winning nation and removed from the losing one (applyBattle) --
		// capture points are the squad-level view of that same occupation swing, so making it zero-sum here
		// keeps the squad board consistent with the war map.
		//
		// Renown is NOT debited: it only decreases when a member leaves (see DebitDepartingMember).
		dec := bson.M{"battles.played": int32(1)}
		if capturePoints > 0 {
			var st SquadStats
			if err := r.stats.FindOne(ctx, bson.M{"teamId": loserTeam}).Decode(&st); err != nil {
				if !errors.Is(err, mongo.ErrNoDocuments) {
					return err
				}
				// No stats doc: nothing banked to forfeit, so just record the loss.
			} else {
				dTotal, dSeason := debitBucket(st.CapturePoints, season, capturePoints)
				if dTotal > 0 {
					dec["capturePoints.total"] = -dTotal
				}
				if dSeason > 0 {
					dec["capturePoints.bySeason."+season] = -dSeason
				}
			}
		}
		if _, err := r.stats.UpdateOne(ctx, bson.M{"teamId": loserTeam}, bson.M{"$inc": dec}, options.UpdateOne().SetUpsert(true)); err != nil {
			return err
		}
	}
	return nil
}

// debitBucket returns how much may actually be taken from a StatBucket's running total and its season
// sub-total, each floored at 0 and computed independently.
//
// The two can legitimately diverge -- a squad that earned in an earlier season has a total larger than its
// current-season bucket -- so a blind negative $inc could drive one negative. A negative bucket would sort
// the squad BELOW squads that never played, which reads as a bug on the ranking board.
func debitBucket(b StatBucket, season string, amount int32) (dTotal, dSeason int32) {
	if amount <= 0 {
		return 0, 0
	}
	dTotal = min(amount, max(b.Total, 0))
	dSeason = min(amount, max(b.BySeason[season], 0))
	return dTotal, dSeason
}

// CreditMemberContributions splits one battle's renown among the pilots who actually fought (the report's
// WinnerUserIDs) and adds each one's share to their per-member RenownContribution ledger. Equal split,
// floored -- the remainder favours the squad, matching the debit's flooring. No-op for no fighters or
// non-positive renown. A pilot in the report who is not on the roster simply matches nothing.
func (r *SquadRepository) CreditMemberContributions(ctx context.Context, teamID string, userIDs []string, renown int32) error {
	if renown <= 0 || len(userIDs) == 0 {
		return nil
	}
	share := renown / int32(len(userIDs))
	if share <= 0 {
		return nil
	}
	for _, uid := range userIDs {
		// Positional $ updates the one member whose userId matches; each pilot appears once on the roster.
		if _, err := r.squads.UpdateOne(ctx,
			bson.M{"teamId": teamID, "members.userId": uid},
			bson.M{"$inc": bson.M{"members.$.renownContribution": share}}); err != nil {
			return err
		}
	}
	return nil
}

// DebitMemberContribution removes exactly the departing member's own tracked contribution from the squad's
// renown (see SquadMemberRecord.RenownContribution). A member who never fought has 0 and costs the squad
// nothing -- the fix for the old flat Renown/N debit that docked non-participants.
//
// Both the running total and the current season bucket are debited, each floored at 0: the buckets can
// diverge (a squad that earned in an earlier season has a total larger than its season bucket), so the
// decrement is computed per-bucket in Go rather than as a blind negative $inc that could drive one negative
// and rank the squad below squads that never played.
func (r *SquadRepository) DebitMemberContribution(ctx context.Context, teamID string, contribution int32, season string) error {
	if contribution <= 0 {
		return nil
	}
	var stats SquadStats
	if err := r.stats.FindOne(ctx, bson.M{"teamId": teamID}).Decode(&stats); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil // never fought, nothing to debit
		}
		return err
	}

	dTotal := min(contribution, max(stats.Renown.Total, 0))
	dSeason := min(contribution, max(stats.Renown.BySeason[season], 0))
	if dTotal == 0 && dSeason == 0 {
		return nil
	}

	dec := bson.M{}
	if dTotal > 0 {
		dec["renown.total"] = -dTotal
	}
	if dSeason > 0 {
		dec["renown.bySeason."+season] = -dSeason
	}
	_, err := r.stats.UpdateOne(ctx, bson.M{"teamId": teamID}, bson.M{"$inc": dec})
	return err
}

// AllTeamIDs returns every real squad's team id. Used by out-of-band tooling (cmd/seedbattles) that needs
// to enumerate squads; the game path always works from a specific team id.
func (r *SquadRepository) AllTeamIDs(ctx context.Context) ([]string, error) {
	cur, err := r.squads.Find(ctx, bson.M{}, options.Find().SetProjection(bson.M{"teamId": 1}))
	if err != nil {
		return nil, err
	}
	var docs []struct {
		TeamID string `bson:"teamId"`
	}
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(docs))
	for _, d := range docs {
		if isRealTeam(d.TeamID) {
			ids = append(ids, d.TeamID)
		}
	}
	return ids, nil
}

// RankEntry is one ranked squad for the leaderboard (1262).
type RankEntry struct {
	TeamID string
	Name   string
	Value  int32
	// Nation is the squad's faction as the wire expects it: 'A' | 'B' | 'C' (the same code the 184 login
	// record calls Country Code). The ranking block carries one per entry and the client renders it as the
	// nation icon; the A/B/C ordering matches icon_Nation1/2/3.
	Nation byte
	// Grade is the squad grade index rendered as FMG string 5700+Grade, one per ranking entry -- the same
	// derivation the squad panel uses (see squadGrade.go).
	Grade byte
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
		nation := byte('A')
		if len(sq.Faction) > 0 && sq.Faction[0] >= 'A' && sq.Faction[0] <= 'C' {
			nation = sq.Faction[0]
		}
		entries = append(entries, RankEntry{
			TeamID: sq.TeamID,
			Name:   sq.Name,
			Value:  val,
			Nation: nation,
			// Derived from lifetime renown rather than the stored Squad.Grade so the board agrees with the
			// squad panel even when a battle credit landed without a RefreshSquadGrade.
			Grade: gradeFromRenown(st.Renown.Total),
		})
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
	if _, err := r.stats.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "teamId", Value: 1}},
		Options: options.Index().SetUnique(true),
	}); err != nil {
		return err
	}
	// Squad history is read newest-first per squad; index (teamId, createdAt desc) covers that query.
	_, err := r.history.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "teamId", Value: 1}, {Key: "createdAt", Value: -1}},
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

// playerGamertag returns the authoritative gamertag for an xuid from the webservices `players` login
// collection (set from the console's own login), or "" if there is no record / no players collection.
// Best-effort and read-only: any lookup error yields "" so the caller falls back to its own value and the
// join is never blocked. Used to avoid persisting the 182 body's mis-sourced (host-stamped) gamertag.
func (r *SquadRepository) playerGamertag(ctx context.Context, xuid string) string {
	if r.players == nil || xuid == "" {
		return ""
	}
	var pl struct {
		Gamertag string `bson:"gamertag"`
	}
	if err := r.players.FindOne(ctx, bson.M{"xuid": xuid}).Decode(&pl); err != nil {
		return ""
	}
	return pl.Gamertag
}

// ProfileByXUID returns a player's persistent profile, or (nil, nil) if they have none yet. Unlike
// EnsureProfile it never mints -- used to resolve the requester's own assigned User ID (e.g. the squad
// host's US, to echo back in the 182 join reply) without side effects.
func (r *SquadRepository) ProfileByXUID(ctx context.Context, xuid string) (*CombasProfile, error) {
	var p CombasProfile
	err := r.profiles.FindOne(ctx, bson.M{"xuid": xuid}).Decode(&p)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// RemoveMember withdraws a member from a squad, returning the status byte the client expects (parser
// sub_823BDB88): '1' Delete Complete, '2' Leader Can't Delete, '3' Target Data None. A leader may only
// leave when they are the last member (the squad then disbands); a leader abandoning remaining members
// is rejected with '2'.
//
// The member is resolved by XUID first (the reliable packet-header identity of the leaving console),
// falling back to the body User ID only when no XUID match is found. This is essential because a joiner
// commonly carries the LEADER's US in its own myIdentity -- the title derives a joiner's identity from
// the shared squad data and adopts the leader's US (confirmed 2026-07-07: both consoles' saves stored
// US0001000000000001) -- so the withdraw body's User ID points at the leader. Keying on the requester's
// XUID removes the correct console; keying on the body US would try to remove the leader and wedge on the
// leader-with-members guard, so neither console could ever leave. 183 is always a self-leave (no kick
// path), so the header XUID is exactly the departing member.
func (r *SquadRepository) RemoveMember(ctx context.Context, teamID, xuid, userID string) (byte, error) {
	sq, err := r.SquadByTeamID(ctx, teamID)
	if err != nil {
		return 0, err
	}
	if sq == nil {
		return '3', nil // Target Data None
	}

	idx := -1
	if xuid != "" {
		for i, m := range sq.Members {
			if m.XUID == xuid {
				idx = i
				break
			}
		}
	}
	if idx == -1 { // fall back to the (less reliable) body User ID
		for i, m := range sq.Members {
			if m.UserID == userID {
				idx = i
				break
			}
		}
	}
	if idx == -1 {
		return '3', nil
	}
	if sq.Members[idx].Leader && len(sq.Members) > 1 {
		return '2', nil // Leader Can't Delete (would orphan the remaining members)
	}

	// Identify the matched member by its STORED ids -- never the passed-in userID, which may be the
	// mis-adopted leader US.
	memberXUID := sq.Members[idx].XUID
	memberUserID := sq.Members[idx].UserID
	memberName := historyName(sq.Members[idx].Name, sq.Members[idx].Gamertag)
	if len(sq.Members) <= 1 {
		// Last member leaving -> disband the squad entirely.
		if _, err := r.squads.DeleteOne(ctx, bson.M{"teamId": teamID}); err != nil {
			return 0, err
		}
	} else {
		if _, err := r.squads.UpdateOne(ctx,
			bson.M{"teamId": teamID},
			bson.M{"$pull": bson.M{"members": bson.M{"userId": memberUserID}}, "$inc": bumpSeq},
		); err != nil {
			return 0, err
		}
	}

	// Unlink the departing member's persistent profile from the team.
	if memberXUID != "" {
		_, _ = r.profiles.UpdateOne(ctx, bson.M{"xuid": memberXUID}, bson.M{"$set": bson.M{"teamId": ""}})
	}
	// Log a "left the squad" history event (type 3). Best-effort, same as the join hook. When the last
	// member leaves the squad disbands (doc deleted above); the history rows survive that deletion so a
	// re-created squad of the same id would inherit stale history -- acceptable since ids are monotonic and
	// never reused (see formatTeamID).
	_ = r.RecordSquadLeft(ctx, teamID, memberName, time.Now())

	// A departure costs the squad the leaver's share of its renown, which can drop it a grade. Best-effort
	// and after the roster write: a failure here must not turn a completed withdraw into an error the
	// client retries, since the member is already gone. Skipped when the squad disbanded (doc deleted).
	if len(sq.Members) > 1 {
		if err := r.DebitMemberContribution(ctx, teamID, sq.Members[idx].RenownContribution, currentSeason); err != nil {
			logging.Warn.Printf("renown debit failed for %s after member left: %v", teamID, err)
		} else if _, err := r.RefreshSquadGrade(ctx, teamID); err != nil {
			logging.Warn.Printf("grade refresh failed for %s after member left: %v", teamID, err)
		}
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
// Idempotent on XUID: a player already on the roster gets their existing User ID back with '1', checked
// across the WHOLE roster before any name/fullness rejection so a re-commit can never be wrongly refused.
//
// XUID is the only reliable identity in the 182 body -- the host mis-sources the gamertag/name fields
// (observed pulling them from the squad lead / a prior member), so uniqueness/idempotency must key on XUID.
// The '3' "User Name Unique Error" is enforced only for a genuinely-new XUID with a non-empty in-squad
// name colliding with a different member (not the Live gamertag: gamertags collide across consoles in
// local testing).
func (r *SquadRepository) AddMember(ctx context.Context, teamID, xuid, gamertag, name string, rank int32) (byte, string, error) {
	sq, err := r.SquadByTeamID(ctx, teamID)
	if err != nil {
		return 0, "", err
	}
	if sq == nil {
		return '0', "", nil // target squad does not exist -> "Unknown Error"
	}

	// Idempotency FIRST, across the whole roster: a player already on the roster gets their existing
	// User ID back regardless of name/gamertag. XUID is the only reliable identity field -- the host
	// sources the gamertag/name fields of the code-182 commit unreliably (observed mis-sourced from the
	// squad lead / a prior member), so those must never reject a re-commit of an existing member.
	// (The previous single loop returned '3' as soon as it hit a member sharing the incoming name, which
	// could fire before this player's own entry was reached and wedge a still-present member's re-commit
	// during join churn.)
	for _, m := range sq.Members {
		if m.XUID == xuid {
			return '1', m.UserID, nil
		}
	}

	// Genuinely new member: enforce in-squad name uniqueness against the OTHER members only, and only
	// for a non-empty name (an empty/omitted name carries no uniqueness constraint).
	if name != "" {
		for _, m := range sq.Members {
			if m.Name == name {
				return '3', "", nil // User Name Unique Error (in-squad name already taken)
			}
		}
	}
	if len(sq.Members) >= squadMemberCap {
		return '2', "", nil // Member Number Over Error (squad full)
	}

	// Don't trust the 182 body's gamertag: the host builder mis-sources it (stamps the host's own tag onto
	// joiners -- traced in IDA). Prefer, in order, the webservices `players` login (authoritative) then the
	// body only as a last resort, for a BRAND-NEW profile. EnsureProfile keeps an EXISTING profile's own
	// gamertag (their prior self-report) regardless of what we pass, so the roster row below takes that.
	insertGT := gamertag
	if auth := r.playerGamertag(ctx, xuid); auth != "" {
		insertGT = auth
	}
	profile, err := r.EnsureProfile(ctx, xuid, insertGT)
	if err != nil {
		return 0, "", err
	}
	// The profile's own tag is authoritative (never the mis-sourced 182 value); fall back to the resolved
	// insert tag only if an existing profile somehow carries a blank gamertag.
	memberGT := profile.Gamertag
	if memberGT == "" {
		memberGT = insertGT
	}

	member := SquadMemberRecord{
		XUID:       xuid,
		UserID:     profile.UserID,
		Gamertag:   memberGT,
		Name:       name,
		Leader:     false,
		UserNumber: firstFreeUserNumber(sq.Members),
		Rank:       rank,
	}
	// Churn-safe append: only push when this XUID is not already on the roster, so a concurrent commit
	// (the host may re-fire 182 during a retry storm) cannot create a duplicate roster entry.
	res, err := r.squads.UpdateOne(ctx,
		bson.M{"teamId": teamID, "members.xuid": bson.M{"$ne": xuid}},
		bson.M{"$push": bson.M{"members": member}, "$inc": bumpSeq},
	)
	if err != nil {
		return 0, "", err
	}
	if res.MatchedCount == 0 {
		// The XUID was added concurrently (or the squad vanished between read and write); return the
		// existing membership idempotently rather than failing the join.
		if sq2, err := r.SquadByTeamID(ctx, teamID); err == nil && sq2 != nil {
			for _, m := range sq2.Members {
				if m.XUID == xuid {
					return '1', m.UserID, nil
				}
			}
		}
		return '0', "", nil
	}
	if _, err := r.profiles.UpdateOne(ctx,
		bson.M{"xuid": xuid},
		bson.M{"$set": bson.M{"teamId": teamID}},
	); err != nil {
		return 0, "", err
	}
	// Single-squad invariant: joining removes the player from any squad they were still in (going forward
	// this stops the multi-squad accumulation; existing dupes are cleaned by cmd/reconcile-squads).
	r.leaveOtherSquads(ctx, xuid, teamID)
	// Log a "joined the squad" history event (type 2). Only on a genuine new-member push -- an idempotent
	// re-commit of an existing member returns above and records nothing. Best-effort: the join itself has
	// already succeeded and must return its status regardless of the history write.
	_ = r.RecordSquadJoined(ctx, teamID, historyName(name, memberGT), time.Now())
	return '1', profile.UserID, nil
}

// SetMemberNumber assigns a member's "hound number" (0..99, unique within the squad), returning the
// status byte the client expects (parser sub_823BDA28): '1' Member Config Complete, '2' Already Been
// Used By Other Users, '3' Target Member Not Exist.
func (r *SquadRepository) SetMemberNumber(ctx context.Context, teamID, xuid, userID string, number byte) (byte, error) {
	sq, err := r.SquadByTeamID(ctx, teamID)
	if err != nil {
		return 0, err
	}
	if sq == nil {
		return '3', nil // Target Member Not Exist
	}

	// Resolve the acting member by XUID first (the reliable packet-header identity), falling back to the
	// body User ID. 204 is a SELF-op (a member sets its OWN hound number), so the header XUID is the acting
	// console; the body US can be the mis-adopted leader US, which would otherwise set the WRONG member's
	// number. Same reliable-identity principle as AddMember / RemoveMember.
	idx := -1
	if xuid != "" {
		for i, m := range sq.Members {
			if m.XUID == xuid {
				idx = i
				break
			}
		}
	}
	if idx == -1 {
		for i, m := range sq.Members {
			if m.UserID == userID {
				idx = i
				break
			}
		}
	}
	if idx == -1 {
		return '3', nil // Target Member Not Exist
	}

	// Collision: any OTHER member already holds this number (the acting member re-claiming its own is fine).
	for i, m := range sq.Members {
		if i != idx && m.UserNumber == int32(number) {
			return '2', nil // Already Been Used By Other Users
		}
	}

	// Update by the matched member's STORED User ID -- never the passed-in userID, which may be the wrong US.
	if _, err := r.squads.UpdateOne(ctx,
		bson.M{"teamId": teamID, "members.userId": sq.Members[idx].UserID},
		bson.M{"$set": bson.M{"members.$.userNumber": int32(number)}, "$inc": bumpSeq},
	); err != nil {
		return 0, err
	}
	return '1', nil // Member Config Complete
}

// SetEmblems stores the raw 192-byte emblem blob on a squad, returning whether the squad existed.
func (r *SquadRepository) SetEmblems(ctx context.Context, teamID string, emblems []byte) (bool, error) {
	res, err := r.squads.UpdateOne(ctx,
		bson.M{"teamId": teamID},
		bson.M{"$set": bson.M{"emblems": emblems}, "$inc": bumpSeq},
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

// NationPlayerCounts is the active-players-by-nation report. ONLINE = players CURRENTLY IN the game,
// resolved to their squad's nation; REGISTERED = every squad member per nation. Keys are nation chars
// "A"/"B"/"C"; ONLINE also carries "none" for an in-game player not in any squad. Read-only aggregate --
// safe to serve to a public dashboard.
//
// "In game" is keyed on the Xenia `players.titleId` == the ChromeHounds title id: it is the reliable signal
// (0/unset when the console is offline or on the dashboard). The stored `state` is NOT reliable -- Xenia
// derives true liveness from its live WS registry and leaves stale state on the 1-day-TTL login record.
type NationPlayerCounts struct {
	Online     map[string]int `json:"online"`
	Registered map[string]int `json:"registered"`
}

// PlayersByNation computes both metrics in one call from the shared combas DB. onlineTitleID filters the
// ONLINE count to players in that title (the ChromeHounds title id); "" counts all login records (not
// recommended -- includes offline consoles still within the 1-day login TTL).
func (r *SquadRepository) PlayersByNation(ctx context.Context, onlineTitleID string) (NationPlayerCounts, error) {
	// Pre-seed the three nations so the payload always has a stable A/B/C shape (0 when none online/registered).
	out := NationPlayerCounts{
		Online:     map[string]int{"A": 0, "B": 0, "C": 0},
		Registered: map[string]int{"A": 0, "B": 0, "C": 0},
	}

	// ONLINE: keep only players currently in the game (titleId), then join to their squad's faction.
	onlinePipe := mongo.Pipeline{}
	if onlineTitleID != "" {
		onlinePipe = append(onlinePipe, bson.D{{Key: "$match", Value: bson.M{"titleId": onlineTitleID}}})
	}
	onlinePipe = append(onlinePipe,
		bson.D{{Key: "$lookup", Value: bson.M{"from": squadsCollection, "localField": "xuid", "foreignField": "members.xuid", "as": "sq"}}},
		bson.D{{Key: "$group", Value: bson.M{
			"_id": bson.M{"$ifNull": bson.A{bson.M{"$arrayElemAt": bson.A{"$sq.faction", 0}}, "none"}},
			"n":   bson.M{"$sum": 1},
		}}},
	)
	onlineCur, err := r.players.Aggregate(ctx, onlinePipe)
	if err != nil {
		return out, err
	}
	if err := decodeNationCounts(ctx, onlineCur, out.Online); err != nil {
		return out, err
	}

	// REGISTERED: sum each squad's member count into its faction.
	regCur, err := r.squads.Aggregate(ctx, mongo.Pipeline{
		{{Key: "$group", Value: bson.M{
			"_id": "$faction",
			"n":   bson.M{"$sum": bson.M{"$size": bson.M{"$ifNull": bson.A{"$members", bson.A{}}}}},
		}}},
	})
	if err != nil {
		return out, err
	}
	if err := decodeNationCounts(ctx, regCur, out.Registered); err != nil {
		return out, err
	}
	return out, nil
}

// decodeNationCounts folds a {_id: <faction|null>, n: <count>} cursor into a nation->count map ("none" for a
// null/empty faction).
func decodeNationCounts(ctx context.Context, cur *mongo.Cursor, into map[string]int) error {
	defer cur.Close(ctx)
	for cur.Next(ctx) {
		var row struct {
			ID any `bson:"_id"`
			N  int `bson:"n"`
		}
		if err := cur.Decode(&row); err != nil {
			return err
		}
		key := "none"
		if s, ok := row.ID.(string); ok && s != "" {
			key = s
		}
		into[key] += row.N
	}
	return cur.Err()
}

// Allegiance-change status bytes, exactly as the client's msgCode-201 parser sub_823BE000 reads them from
// the first response body byte: '1' Complete, '2' "Demand Same As Current State", anything else "Unknown
// Error". allegianceError is any non-1/2 value.
const (
	allegianceComplete  byte = '1'
	allegianceSameState byte = '2'
	allegianceError     byte = '9'
)

// ChangeAllegiance moves a squad to a new nation ('A' Tarakia / 'B' Morskoj / 'C' Sal Kar) -- the
// season-start allegiance switch (msgCode 201), which is DISTINCT from deadFlag-driven defection. It returns
// the wire status byte: '1' if the faction was changed, '2' if the squad already belongs to that nation
// (idempotent -- a repeated request is "same state", not a fresh change), and allegianceError ('9' ->
// "Unknown Error") for an unknown squad or an out-of-range nation. A non-nil error is reserved for genuine
// datastore faults so the caller can distinguish "reject the request" from "we failed to serve it".
func (r *SquadRepository) ChangeAllegiance(ctx context.Context, teamID string, nation byte) (byte, error) {
	if nation < 'A' || nation > 'C' {
		return allegianceError, nil // bad target nation -> Unknown Error (a client/protocol issue, not ours)
	}
	sq, err := r.SquadByTeamID(ctx, teamID)
	if err != nil {
		return allegianceError, err // datastore fault
	}
	if sq == nil {
		return allegianceError, nil // no such squad -> Unknown Error
	}
	if len(sq.Faction) > 0 && sq.Faction[0] == nation {
		return allegianceSameState, nil // already this nation
	}
	if _, err := r.squads.UpdateOne(ctx, bson.M{"teamId": teamID}, bson.M{"$set": bson.M{"faction": string(nation)}}); err != nil {
		return allegianceError, err // datastore fault
	}
	logging.Info.Printf("[SQUAD] allegiance change: %s %q -> %q", teamID, sq.Faction, string(nation))
	return allegianceComplete, nil
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
		Grade:   squadGradeMin, // every squad starts at the lowest grade
		Members: []SquadMemberRecord{{
			XUID:       leader.XUID,
			UserID:     leader.UserID,
			Gamertag:   leader.Gamertag,
			Leader:     true,
			UserNumber: 1,
			Rank:       1,
		}},
		UpdateSeq: 1, // first serial; the leader's first login reads count 1 > cached 0 -> VALID
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
	// Single-squad invariant: creating a squad removes the creator from any squad they were still in.
	r.leaveOtherSquads(ctx, leader.XUID, squad.TeamID)
	return squad, nil
}
