package server

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Squad-history event types (Release.xex sub_821C8D10 UI reader; template = MenuText FMG 5720+type).
// The wire record's u16 TYPE selects the client-side story template; type 0 is the end-of-list
// terminator. See squadHistoryServer.go for the wire packing and the [C-RE] offset catalog.
const (
	historyTypeTerminator = 0 // 5720 %null% -- end-of-list marker (never stored, only emitted on the wire)
	historyTypeJoined     = 2 // 5722 "%s has joined the squad"          (content = member name @+9)
	historyTypeLeft       = 3 // 5723 "%s has left the squad"            (content = member name @+9)
	historyTypeGradeUp    = 4 // 5724 "The squad grade has gone up to %s" (content = grade idx @+9 -> FMG 5700+idx)
	historyTypeGradeDown  = 5 // 5725 "The squad grade has gone down to %s"
	historyTypeInvading   = 6 // 5726 "Invading %s"                       (content = area/map ids @+105/+108)
	historyTypeDefense    = 7 // 5727 "Defensive deployment to %s"        (content = area/map ids @+105/+108)
)

// SquadHistoryEvent is one dated entry in a squad's history feed. Only the fields relevant to the event
// TYPE are populated; the wire packer (historyRecord) reads the right ones per type.
type SquadHistoryEvent struct {
	TeamID    string `bson:"teamId"`
	Type      int32  `bson:"type"`               // one of historyType* (2..7)
	CreatedAt int64  `bson:"createdAt"`          // unix seconds; the feed is newest-first
	Name      string `bson:"name,omitempty"`     // type 2/3: the joining/leaving member name (UTF-8, <=32ch)
	GradeIdx  int32  `bson:"gradeIdx,omitempty"` // type 4/5: the new grade index (1..13 -> FMG 5700+idx)
	AreaID    int32  `bson:"areaId,omitempty"`   // type 6/7: battle location area id (map-name lookup arg)
	MapID     int32  `bson:"mapId,omitempty"`    // type 6/7: battle location map id (map-name lookup arg)
}

// historyName picks the in-squad pilot name when set, else the Live gamertag. The squad-history "%s
// joined/left" story shows the in-squad name (matching how squadLoginStateFromSquad prefers rec.Name),
// falling back to the gamertag for members with no separate pilot name (e.g. the leader).
func historyName(name, gamertag string) string {
	if name != "" {
		return name
	}
	return gamertag
}

// RecordSquadHistory appends one event to a squad's history. Best-effort at the call sites: a failure here
// never blocks the squad op that triggered it (join/leave already returned their status).
func (r *SquadRepository) RecordSquadHistory(ctx context.Context, ev SquadHistoryEvent) error {
	if ev.CreatedAt == 0 {
		ev.CreatedAt = time.Now().Unix()
	}
	_, err := r.history.InsertOne(ctx, ev)
	return err
}

// RecordSquadJoined logs a "<name> has joined the squad" event (type 2).
func (r *SquadRepository) RecordSquadJoined(ctx context.Context, teamID, name string) error {
	if !isRealTeam(teamID) {
		return nil // AI/CPU placeholder squads keep no history
	}
	return r.RecordSquadHistory(ctx, SquadHistoryEvent{TeamID: teamID, Type: historyTypeJoined, Name: name})
}

// RecordSquadLeft logs a "<name> has left the squad" event (type 3).
func (r *SquadRepository) RecordSquadLeft(ctx context.Context, teamID, name string) error {
	if !isRealTeam(teamID) {
		return nil
	}
	return r.RecordSquadHistory(ctx, SquadHistoryEvent{TeamID: teamID, Type: historyTypeLeft, Name: name})
}

// RecordSquadBattle logs an "Invading <map>" (type 6) or "Defensive deployment to <map>" (type 7) event
// for a squad's mission. invading distinguishes an attack on an area the squad's nation does not hold from
// a defence of its own. NOT yet wired into the battle-report ingest -- the attacker/defender split needs
// the fought area's owner at battle time, which lives in the BattleApplier; see the TODO in
// squadHistoryServer.go and server.md S-Q. Kept here so the ingest hook is a one-line call once confirmed.
func (r *SquadRepository) RecordSquadBattle(ctx context.Context, teamID string, areaID, mapID int32, invading bool) error {
	if !isRealTeam(teamID) {
		return nil
	}
	t := int32(historyTypeDefense)
	if invading {
		t = historyTypeInvading
	}
	return r.RecordSquadHistory(ctx, SquadHistoryEvent{TeamID: teamID, Type: t, AreaID: areaID, MapID: mapID})
}

// RecentSquadHistory returns a squad's newest events first, capped at limit. An empty history (or unknown
// squad) yields an empty slice, which the server serves as status 0 + a lone type-0 terminator (the
// graceful "no history" reply, like the count-0 news path).
func (r *SquadRepository) RecentSquadHistory(ctx context.Context, teamID string, limit int) ([]SquadHistoryEvent, error) {
	cur, err := r.history.Find(ctx,
		bson.M{"teamId": teamID},
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}).SetLimit(int64(limit)),
	)
	if err != nil {
		return nil, err
	}
	var evs []SquadHistoryEvent
	if err := cur.All(ctx, &evs); err != nil {
		return nil, err
	}
	return evs, nil
}
