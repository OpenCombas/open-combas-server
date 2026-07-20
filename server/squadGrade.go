package server

import (
	"context"
	"errors"
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
