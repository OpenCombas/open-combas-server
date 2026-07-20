package server

import (
	"ChromehoundsStatusServer/logging"
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const maintenanceCollection = "serverMaintenance"

// maintenanceDocID is the fixed _id of the single maintenance document -- there is one window server-wide,
// so a singleton doc keeps "set the maintenance window" a one-line upsert.
const maintenanceDocID = "current"

// MaintenanceWindow is the announced maintenance period served in the status reply (msgCode 187).
type MaintenanceWindow struct {
	ID    string    `bson:"_id"`
	Start time.Time `bson:"start"`
	End   time.Time `bson:"end"`
}

// The client derives THREE states from (now, start, end) in Release.xex sub_823B5FC8, and the announce gate
// sub_82198EC8 turns them into popups:
//
//	now <  start (beyond ~15 min)  -> all three flags 0 -> gate FALLS THROUGH -> dated "server will be out
//	                                  of service from HH:MM on MM/DD/YYYY" popup (FMG 2505, dialog 5349)
//	now <  start (within ~15 min)  -> +2900 "approaching" -> countdown popup (FMG 2507, dialog 5345)
//	start <= now <= end            -> +2904 "in maintenance" -> in-maintenance announce
//	now >  end                     -> +2908 "ended" -> gate returns early, NOTHING SHOWN
//
// The counter-intuitive consequence: a window far in the FUTURE is the one setting guaranteed to nag every
// login, and pushing it further out does not help -- the ~15 minute announce thresholds (dword_82059EEC =
// 900/600/300/60) gate only the countdown, never the dated announce.
//
// So "no maintenance scheduled" must be expressed as a window that has ALREADY PASSED, which is what
// silentPastWindow returns.
const (
	// silentWindowStartAgo/EndAgo place the default window comfortably in the past. The margin absorbs
	// clock skew between us and the console's server-synced clock (seeded from ServerLocalTime and ticked
	// forward locally, Release.xex +2864); if `now` ever landed before `end` the client would believe
	// maintenance was in progress and boot everyone to the title screen.
	silentWindowStartAgo = 48 * time.Hour
	silentWindowEndAgo   = 24 * time.Hour
)

// silentPastWindow is the "nothing scheduled" window: entirely in the past, so the client computes
// "maintenance ended" and announces nothing.
func silentPastWindow(now time.Time) (start, end time.Time) {
	return now.Add(-silentWindowStartAgo), now.Add(-silentWindowEndAgo)
}

// MaintenanceWindowFor resolves the window to serve at time `now`.
//
// A stored window is served verbatim while it is still relevant -- that is what makes the client announce
// it -- but once it has fully elapsed it is indistinguishable from the silent default, so it is simply
// served as-is (already in the past = silent). A repo-less or empty deployment gets the silent default.
func (r *WorldRepository) MaintenanceWindowFor(ctx context.Context, now time.Time) (start, end time.Time) {
	start, end = silentPastWindow(now)
	if r == nil || r.maintenance == nil {
		return start, end
	}

	var w MaintenanceWindow
	if err := r.maintenance.FindOne(ctx, bson.M{"_id": maintenanceDocID}).Decode(&w); err != nil {
		if !errors.Is(err, mongo.ErrNoDocuments) {
			logging.Warn.Printf("maintenance window read failed, announcing none: %v", err)
		}
		return start, end
	}
	if w.Start.IsZero() || w.End.IsZero() || !w.End.After(w.Start) {
		// A malformed window would put the client into an unpredictable state (possibly "in maintenance",
		// which boots players); refuse it and announce nothing.
		logging.Warn.Printf("maintenance window is malformed (start=%v end=%v), announcing none", w.Start, w.End)
		return start, end
	}
	return w.Start.UTC(), w.End.UTC()
}

// SetMaintenanceWindow stores the server-wide maintenance window. Callers are admin tooling, not the game
// path. Passing an end at or before start is rejected rather than stored, since the client cannot render a
// coherent state from it.
func (r *WorldRepository) SetMaintenanceWindow(ctx context.Context, start, end time.Time) error {
	if !end.After(start) {
		return errors.New("maintenance end must be after start")
	}
	_, err := r.maintenance.UpdateOne(ctx,
		bson.M{"_id": maintenanceDocID},
		bson.M{"$set": bson.M{"start": start.UTC(), "end": end.UTC()}},
		options.UpdateOne().SetUpsert(true),
	)
	return err
}

// ClearMaintenanceWindow removes any scheduled window, returning the server to announcing nothing.
func (r *WorldRepository) ClearMaintenanceWindow(ctx context.Context) error {
	_, err := r.maintenance.DeleteOne(ctx, bson.M{"_id": maintenanceDocID})
	return err
}
