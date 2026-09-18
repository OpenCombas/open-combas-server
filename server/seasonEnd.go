package server

// End-of-season rollover: a single operator command (cmd/maintenance -season-end) schedules the whole
// war-ending sequence, and this file drives it automatically off the clock. There are three configurable
// durations (lead, result, maintenance) that define four instants:
//
//	t_cmd ── lead ──▶ t0 ── result ──▶ t1 ── maintenance ──▶ t2
//	                (ends)          (window opens)        (new season live)
//
//   - [t_cmd,t0]  lead:        nothing changes; the war runs normally (lets a season-end be queued ahead).
//   - t0          season ends: lock all maps, fire the end-of-war event (3-way / 2-way truce or a single
//                              nation's victory, from the state AT t0), and announce the maintenance window.
//   - [t0,t1]     result:      season N stays set, maps locked, the story shows, server AVAILABLE.
//   - [t1,t2]     maintenance: season N -> N+1, the battlefield is reset, maps stay locked (server shows
//                              in-maintenance to clients).
//   - t2          new season:  maps unlock and the maintenance window falls back to healthy automatically
//                              (SeasonLocked auto-expires; MaintenanceWindowFor auto-falls-back once elapsed).
//
// The transition applies with no server restart: RunSeasonController ticks in the background, keeping the
// season globals live (RefreshSeasonState) and advancing the schedule (ApplyDueSeasonEnd). The applier is a
// two-phase state machine guarded by LockedAt (phase 1) and AppliedAt (phase 2), so each side effect runs
// exactly once even across ticks or a restart.

import (
	"ChromehoundsStatusServer/logging"
	"ChromehoundsStatusServer/persistence"
	"ChromehoundsStatusServer/reset"
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// seasonEndDocID is the fixed _id of the single season-end schedule document. It lives in the same
// serverConfig collection as the season number/start (seasonDocID), as a separate doc.
const seasonEndDocID = "seasonEnd"

// peaceAccordRow is the WorldSituationInfoNewsParam row for "Neroimus War, Peace Accord" (rows 16-28 are all
// peace-accord variants). Used for both a 2-way and a 3-way truce; a nation-specific accord row can be
// substituted later once the .bin's per-combination rows are mapped.
const peaceAccordRow = int32(16)

// SeasonEndSchedule is the stored plan for one end-of-season rollover (serverConfig doc _id="seasonEnd").
// The three instants are unix seconds; LockedAt/AppliedAt are the phase guards (0 = that phase is pending).
type SeasonEndSchedule struct {
	ID          string `bson:"_id"`
	CreatedAt   int64  `bson:"createdAt"`
	EndsAt      int64  `bson:"endsAt"`      // t0: season ends (lock + event + announce window)
	WindowStart int64  `bson:"windowStart"` // t1: maintenance window opens (season++ + battlefield reset)
	WindowEnd   int64  `bson:"windowEnd"`   // t2: maps unlock, new season live
	FromSeason  int    `bson:"fromSeason"`  // season N that ended
	NextSeason  int    `bson:"nextSeason"`  // season N+1 that begins at t2
	Downscale   int32  `bson:"downscale"`   // battlefield-reset downscale for the new season
	LockedAt    int64  `bson:"lockedAt"`    // 0 until phase 1 (lock/event/window) has run
	Outcome     string `bson:"outcome,omitempty"`
	EventRow    int32  `bson:"eventRow,omitempty"`
	AppliedAt   int64  `bson:"appliedAt"` // 0 until phase 2 (season++ + reset) has run
}

// Pending reports whether the rollover has not yet completed its destructive phase (phase 2). A new
// season-end is refused while one is Pending unless forced.
func (s *SeasonEndSchedule) Pending() bool { return s != nil && s.AppliedAt == 0 }

// LoadSeasonEnd reads the stored schedule (nil when none is set).
func LoadSeasonEnd(ctx context.Context, store *persistence.Store) (*SeasonEndSchedule, error) {
	var s SeasonEndSchedule
	err := store.Collection(serverConfigCollection).FindOne(ctx, bson.M{"_id": seasonEndDocID}).Decode(&s)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// SaveSeasonEnd stores (replaces) the schedule document.
func SaveSeasonEnd(ctx context.Context, store *persistence.Store, s *SeasonEndSchedule) error {
	s.ID = seasonEndDocID
	_, err := store.Collection(serverConfigCollection).ReplaceOne(ctx,
		bson.M{"_id": seasonEndDocID}, s, options.Replace().SetUpsert(true))
	return err
}

// ClearSeasonEnd removes any scheduled rollover (used by -cancel-season-end).
func ClearSeasonEnd(ctx context.Context, store *persistence.Store) error {
	_, err := store.Collection(serverConfigCollection).DeleteOne(ctx, bson.M{"_id": seasonEndDocID})
	return err
}

// stampSeasonEnd sets fields on the schedule doc (the phase guards + the resolved outcome).
func stampSeasonEnd(ctx context.Context, store *persistence.Store, fields bson.M) error {
	_, err := store.Collection(serverConfigCollection).UpdateOne(ctx,
		bson.M{"_id": seasonEndDocID}, bson.M{"$set": fields})
	return err
}

// DetermineOutcome classifies the war result from the nations' state: a nation is STANDING when it is
// neither eliminated (DeadFlag) nor dissolved (HQLostTo). 1 standing -> that nation's victory; 2 -> a
// two-way truce; 3 (or the degenerate 0) -> a three-way truce. victor is the nation char for a victory,
// 0 otherwise.
func DetermineOutcome(nations []NationRecord) (kind string, victor byte) {
	var standing []byte
	for _, n := range nations {
		if n.DeadFlag == 0 && n.HQLostTo == "" && len(n.CountryCode) == 1 {
			standing = append(standing, n.CountryCode[0])
		}
	}
	switch len(standing) {
	case 1:
		return "victory", standing[0]
	case 2:
		return "truce2", 0
	default:
		return "truce3", 0
	}
}

// SeasonEndEventRow maps an outcome to the WorldSituationInfoNewsParam row for its story: a victory uses the
// conqueror's "Conquers Neroimus" row (A=10/B=12/C=14), a truce uses the peace-accord row.
func SeasonEndEventRow(kind string, victor byte) (int32, bool) {
	switch kind {
	case "victory":
		return conquestRow(victor)
	case "truce2", "truce3":
		return peaceAccordRow, true
	}
	return 0, false
}

// conquestRow maps a victorious nation to its "<Nation> Conquers Neroimus" param row (A/Tarakia=10,
// B/Morskoj=12, C/Sal Kar=14). Like dissolutionRow, the nation is baked into the text.
func conquestRow(nation byte) (int32, bool) {
	switch nation {
	case 'A':
		return 10, true
	case 'B':
		return 12, true
	case 'C':
		return 14, true
	}
	return 0, false
}

// RefreshSeasonState reloads the season number and between-seasons start from the DB into the in-process
// globals, so SeasonNumber()/SeasonLocked() reflect DB changes WITHOUT a restart. Called each controller
// tick; keeps the per-request path free of Mongo reads.
func RefreshSeasonState(ctx context.Context, store *persistence.Store) error {
	n, err := LoadSeasonNumber(ctx, store)
	if err != nil {
		return err
	}
	ApplySeasonNumber(n)
	ts, err := LoadSeasonStart(ctx, store)
	if err != nil {
		return err
	}
	ApplySeasonStart(ts)
	return nil
}

// ApplyDueSeasonEnd advances the scheduled rollover based on `now`. Idempotent: each phase runs once,
// guarded by the schedule's LockedAt/AppliedAt. Safe to call from the CLI (once) and the controller (each
// tick). A no-op when nothing is scheduled or nothing is due yet.
func ApplyDueSeasonEnd(ctx context.Context, store *persistence.Store, repo *WorldRepository, now time.Time) error {
	if store == nil || repo == nil {
		return nil
	}
	s, err := LoadSeasonEnd(ctx, store)
	if err != nil || s == nil {
		return err
	}
	nowU := now.Unix()

	// Phase 1 (t0): end the season -- lock maps, fire the story, announce the window.
	if nowU >= s.EndsAt && s.LockedAt == 0 {
		if err := seasonEndPhase1(ctx, store, repo, s, nowU); err != nil {
			return err
		}
	}
	// Phase 2 (t1): roll the season over -- increment + reset the battlefield.
	if nowU >= s.WindowStart && s.AppliedAt == 0 {
		if err := seasonEndPhase2(ctx, store, s, nowU); err != nil {
			return err
		}
	}
	return nil
}

// seasonEndPhase1 runs the t0 side effects and stamps LockedAt so it never repeats.
func seasonEndPhase1(ctx context.Context, store *persistence.Store, repo *WorldRepository, s *SeasonEndSchedule, nowU int64) error {
	nations, err := repo.Nations(ctx)
	if err != nil {
		return err
	}
	kind, victor := DetermineOutcome(nations)
	row, ok := SeasonEndEventRow(kind, victor)
	if ok {
		if err := repo.RecordEvent(ctx, EventRecord{CreatedAt: nowU, TemplateID: row}); err != nil {
			logging.Warn.Printf("[SEASON-END] record end-of-war event (row %d) failed: %v", row, err)
		}
	} else {
		logging.Warn.Printf("[SEASON-END] no event row for outcome %q (victor %q); firing none", kind, string(victor))
	}
	// Lock every map from now until t2 (the whole result + maintenance span). SeasonLocked() reads the
	// clock, so it auto-lifts at t2. Set the DB value AND the in-process global (the latter matters when
	// this runs inside the long-lived server; the CLI process's global is discarded on exit).
	if err := SaveSeasonStart(ctx, store, s.WindowEnd); err != nil {
		return err
	}
	ApplySeasonStart(s.WindowEnd)
	// Announce the maintenance window [t1,t2]; served live and self-clearing once elapsed.
	if err := repo.SetMaintenanceWindow(ctx, time.Unix(s.WindowStart, 0), time.Unix(s.WindowEnd, 0)); err != nil {
		return err
	}
	if err := stampSeasonEnd(ctx, store, bson.M{"lockedAt": nowU, "outcome": kind, "eventRow": row}); err != nil {
		return err
	}
	s.LockedAt, s.Outcome, s.EventRow = nowU, kind, row
	logging.Info.Printf("[SEASON-END] season %d ended: outcome=%s (victor=%q, news row %d); maps LOCKED until %s; maintenance window %s..%s announced",
		s.FromSeason, kind, string(victor), row,
		time.Unix(s.WindowEnd, 0).Format(time.RFC3339), time.Unix(s.WindowStart, 0).Format(time.RFC3339), time.Unix(s.WindowEnd, 0).Format(time.RFC3339))
	return nil
}

// seasonEndPhase2 runs the t1 destructive rollover and stamps AppliedAt so it never repeats. The map lock
// (SeasonStartsAt=t2, set in phase 1) is untouched -- reset.Reset does not change it -- so maps stay locked
// through the maintenance window and lift at t2.
func seasonEndPhase2(ctx context.Context, store *persistence.Store, s *SeasonEndSchedule, nowU int64) error {
	if err := reset.Reset(ctx, store, s.Downscale, "all"); err != nil {
		return err
	}
	if err := SaveSeasonNumber(ctx, store, s.NextSeason); err != nil {
		return err
	}
	ApplySeasonNumber(s.NextSeason)
	if err := stampSeasonEnd(ctx, store, bson.M{"appliedAt": nowU}); err != nil {
		return err
	}
	s.AppliedAt = nowU
	logging.Info.Printf("[SEASON-END] maintenance rollover applied: season %d -> %d, battlefield reset (downscale %d); maps unlock at %s",
		s.FromSeason, s.NextSeason, s.Downscale, time.Unix(s.WindowEnd, 0).Format(time.RFC3339))
	return nil
}

// RunSeasonController is the background loop that keeps the season globals live and advances a scheduled
// season-end. It runs one pass immediately (recovering a transition that fell due while the server was
// down) then every `interval`, until ctx is cancelled. Each pass is wrapped in a short timeout so a slow
// Mongo op can't wedge the ticker.
func RunSeasonController(ctx context.Context, store *persistence.Store, repo *WorldRepository, interval time.Duration) {
	if store == nil {
		return
	}
	logging.Info.Printf("[SEASON-END] controller active (season/lock refresh every %s)", interval)
	prevLocked, prevSeason := SeasonLocked(), SeasonNumber()
	pass := func() {
		opCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		if err := RefreshSeasonState(opCtx, store); err != nil {
			logging.Warn.Printf("[SEASON-END] season-state refresh failed: %v", err)
		}
		// Announce any live change so the lock/season transition is observable in the server logs (the
		// per-request path stays silent). This is how an operator confirms a -season-end took hold.
		if locked, season := SeasonLocked(), SeasonNumber(); locked != prevLocked || season != prevSeason {
			if locked {
				logging.Info.Printf("[SEASON-END] state: maps LOCKED, season=%d (until %s)",
					season, time.Unix(SeasonStartsAt(), 0).UTC().Format(time.RFC3339))
			} else {
				logging.Info.Printf("[SEASON-END] state: maps OPEN, season=%d", season)
			}
			prevLocked, prevSeason = locked, season
		}
		if err := ApplyDueSeasonEnd(opCtx, store, repo, time.Now()); err != nil {
			logging.Warn.Printf("[SEASON-END] schedule advance failed: %v", err)
		}
	}
	pass()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			pass()
		}
	}
}
