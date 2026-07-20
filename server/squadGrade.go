package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Squad grade is derived from LIFETIME RENOWN alone (SquadStats.Renown.Total) on a log ladder: each grade
// costs ~70% more renown than the one below it, topping out at 100,000 for grade 13.
//
// Lifetime rather than the season bucket: grade is a standing shown on the squad panel, and a seasonal
// reset would floor every squad at rollover. The season bucket stays on the ranking board (1262), which is
// where seasonal standing belongs.
//
// SCALE CONTEXT: renown per battle is a single byte in the battle report (`b[0x11C]`, so <= 255) and real
// captures show ~150 for a win. So grade 13 is roughly 667 wins -- a lifetime ceiling, not a season goal.
// The low grades are deliberately cheap (grade 2 at two wins) so a new squad sees movement early, while
// 11 -> 13 is where it becomes a real commitment.
//
// This REPLACED a percentile-over-population model. An absolute ladder is a pure function of one number:
// no population query, no index, and no staleness problem where one squad's win silently demotes another.
// Grade still moves in BOTH directions, because renown itself can decrease -- so the title's "grade has
// gone down to %s" history event (squadHistoryRepo type 5) stays reachable and RefreshSquadGrade derives
// the up/down flag from the comparison rather than assuming a promotion.

// gradeThreshold is the minimum lifetime renown required to hold a grade.
type gradeThreshold struct {
	Grade  byte
	Renown int32
}

// squadGradeLadder is ordered HIGHEST FIRST -- gradeFromRenown takes the first threshold met. Values are a
// geometric series from 300 to 100,000 (ratio ~1.6957), rounded to readable numbers.
//
// These cutoffs are OUR CHOICE: the original service's curve is not recoverable from the binary (the client
// only renders FMG 5700+idx and never computes the index itself). Tune freely; the constraints are that
// grades stay within squadGradeMin..squadGradeMax, thresholds strictly descend alongside grades, and the
// lowest entry is squadGradeMin at 0 so every squad has a grade.
var squadGradeLadder = []gradeThreshold{
	{13, 100000}, // ~667 wins
	{12, 59000},
	{11, 35000},
	{10, 21000},
	{9, 12000},
	{8, 7100},
	{7, 4200},
	{6, 2500},
	{5, 1500},
	{4, 900},
	{3, 500},
	{2, 300}, // ~2 wins
	{squadGradeMin, 0},
}

// gradeFromRenown maps lifetime renown to a grade. Negative or zero renown yields squadGradeMin.
func gradeFromRenown(renown int32) byte {
	for _, t := range squadGradeLadder {
		if renown >= t.Renown {
			return clampSquadGrade(int32(t.Grade))
		}
	}
	return squadGradeMin
}

// SquadGradeFor computes a squad's grade from its lifetime renown. A squad with no stats doc has never
// fought, which is the floor rather than an error.
func (r *SquadRepository) SquadGradeFor(ctx context.Context, teamID string) (byte, error) {
	var stats SquadStats
	if err := r.stats.FindOne(ctx, bson.M{"teamId": teamID}).Decode(&stats); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return squadGradeMin, nil
		}
		return squadGradeMin, err
	}
	return gradeFromRenown(stats.Renown.Total), nil
}

// RefreshSquadGrade recomputes a squad's grade, persists it when it has moved, and records the matching
// history event. This is the ranking-driven hook RecordSquadGrade was written for; call it after crediting
// a battle. The up/down flag is derived from the comparison rather than hard-coded to "up" so that
// retuning the ladder downward still records the correct event.
func (r *SquadRepository) RefreshSquadGrade(ctx context.Context, teamID string) (byte, error) {
	grade, err := r.SquadGradeFor(ctx, teamID)
	if err != nil {
		return squadGradeMin, err
	}

	var squad Squad
	if err := r.squads.FindOne(ctx, bson.M{"teamId": teamID}).Decode(&squad); err != nil {
		return grade, err
	}
	old := clampSquadGrade(squad.Grade)
	if old == grade {
		return grade, nil
	}

	if _, err := r.squads.UpdateOne(ctx,
		bson.M{"teamId": teamID},
		bson.M{"$set": bson.M{"grade": int32(grade)}, "$inc": bumpSeq},
	); err != nil {
		return grade, err
	}
	// Zero time -> RecordSquadHistory stamps it with now (see stamp/RecordSquadHistory).
	return grade, r.RecordSquadGrade(ctx, teamID, int32(grade), grade > old, time.Time{})
}

// GradeBackfillReport summarises a BackfillSquadGrades run.
type GradeBackfillReport struct {
	Applied bool
	Scanned int
	Changes []string // human-readable "TM... 'Name': grade 1 -> 7 (renown 4210)"
}

// BackfillSquadGrades syncs every squad's STORED grade with the value derived from its lifetime renown,
// deliberately WITHOUT recording a history event.
//
// WHY THIS EXISTS. Nothing renders the stored grade -- both the squad panel and the ranking board derive it
// at read time -- so grades are already correct without any migration. The stored copy has exactly one
// consumer: the `old != new` comparison in RefreshSquadGrade that decides whether to record a grade-change
// history event.
//
// That makes an unset/stale stored grade a LATENT FALSE EVENT rather than a display bug. clampSquadGrade(0)
// reads as grade 1, so the first battle credit after deploy compares 1 against the squad's real grade and
// writes "grade has gone up to 7" into its history -- a promotion that never happened, for a squad that may
// have held grade 7 for weeks. One per squad, once, and only for squads that fight, which is exactly the
// kind of small persistent lie that is hard to notice and impossible to distinguish later from a real
// promotion.
//
// Running this before the new binary starts crediting battles absorbs the discrepancy silently. It is
// idempotent: a second run reports zero changes.
func (r *SquadRepository) BackfillSquadGrades(ctx context.Context, apply bool) (GradeBackfillReport, error) {
	report := GradeBackfillReport{Applied: apply}

	var squads []Squad
	cur, err := r.squads.Find(ctx, bson.M{})
	if err != nil {
		return report, err
	}
	if err := cur.All(ctx, &squads); err != nil {
		return report, err
	}

	// One read of the whole stats collection rather than a lookup per squad: this runs against every squad
	// in the database, and the per-squad form would be N round-trips for no benefit.
	var stats []SquadStats
	scur, err := r.stats.Find(ctx, bson.M{})
	if err != nil {
		return report, err
	}
	if err := scur.All(ctx, &stats); err != nil {
		return report, err
	}
	renownByTeam := make(map[string]int32, len(stats))
	for _, s := range stats {
		renownByTeam[s.TeamID] = s.Renown.Total
	}

	for _, sq := range squads {
		report.Scanned++
		renown := renownByTeam[sq.TeamID] // absent stats doc -> 0 -> squadGradeMin, which is correct
		want := gradeFromRenown(renown)
		have := clampSquadGrade(sq.Grade)
		if have == want {
			continue
		}
		report.Changes = append(report.Changes,
			fmt.Sprintf("%s %q: stored grade %d -> %d (lifetime renown %d)", sq.TeamID, sq.Name, have, want, renown))

		if !apply {
			continue
		}
		// No bumpSeq here. UpdateSeq drives the client's roster re-accept; a grade correction is not a
		// roster change, and bumping it would make every squad re-propagate on next login for nothing.
		if _, err := r.squads.UpdateOne(ctx,
			bson.M{"teamId": sq.TeamID},
			bson.M{"$set": bson.M{"grade": int32(want)}},
		); err != nil {
			return report, err
		}
	}
	return report, nil
}
