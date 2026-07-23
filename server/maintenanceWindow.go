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

// The client derives three flags from (now, start, end) -- Release.xex sub_823B5FC8, retail sub_82162B48's
// operands -- and TWO separate subsystems consume them:
//
//	window        flag   announce gate                        online available?
//	-----------   -----  -----------------------------------  -----------------
//	future        none   dated "out of service from HH:MM on  YES
//	                     MM/DD/YYYY" (FMG 2505, dialog 5349),
//	                     ONCE per session (latched)
//	< ~15 min     +2900  countdown (FMG 2507, dialog 5345)    yes
//	now inside    +2904  in-maintenance (FMG 2506)            NO
//	past          +2908  nothing                              NO
//
// The availability predicate is the trap. Release.xex sub_82151700 (retail; 7 call sites across the menu
// code) is:
//
//	return !netmgr || netmgr[+3944] || netmgr[+3948] || netmgr[+8] != 0;
//
// It treats "maintenance ENDED" as unavailable exactly like "in maintenance". So a window in the PAST does
// silence the announce -- but only by telling the client the server is offline, which surfaces as "the
// Chromehounds server is currently unavailable due to maintenance". That is strictly worse than the popup
// it removes. (Measured 2026-07-20: an ...-48h/-24h window produced precisely that.)
//
// There is NO encoding for "no maintenance scheduled". All-flags-zero IS the healthy state, and it is the
// state that shows the dated announce. The title assumes a window always exists and tells players about it
// once per session. So the correct default is a window far enough in the FUTURE to be harmless, and the
// once-per-session popup is by design rather than a bug to engineer around.
//
// Offsets differ per build (Preview 2900/2904/2908, retail 3940/3944/3948) but the logic is identical --
// retail sub_82162B48 is structurally the same function as Preview sub_82198EC8.
const (
	// defaultWindowStartIn/EndIn place the default window far enough out that the announced date is
	// plausible and distant. Must stay well beyond the ~15 minute announce thresholds (dword_82059EEC =
	// 900/600/300/60) or the countdown popup starts instead.
	defaultWindowStartIn = 30 * 24 * time.Hour
	defaultWindowEndIn   = defaultWindowStartIn + 2*time.Hour
)

// defaultFutureWindow is the "nothing scheduled" window. It deliberately sits in the future: that is the
// only state in which the client considers the server AVAILABLE (see the table above).
func defaultFutureWindow(now time.Time) (start, end time.Time) {
	return now.Add(defaultWindowStartIn), now.Add(defaultWindowEndIn)
}

// MaintenanceWindowFor resolves the window to serve at time `now`.
//
// A stored window is served verbatim -- that is what makes the client announce it. An ELAPSED stored window
// is deliberately NOT served: once past, it would mark the server unavailable (see the table above), so it
// falls back to the default. A repo-less or empty deployment gets the default too.
func (r *WorldRepository) MaintenanceWindowFor(ctx context.Context, now time.Time) (start, end time.Time) {
	start, end = defaultFutureWindow(now)
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
		// A malformed window would put the client into an unpredictable state; refuse it.
		logging.Warn.Printf("maintenance window is malformed (start=%v end=%v), using default", w.Start, w.End)
		return start, end
	}
	if !w.End.After(now) {
		// Fully elapsed: serving it would set the client's "ended" flag, which its availability predicate
		// treats as the server being OFFLINE. Fall back rather than take the whole server down.
		logging.Info.Printf("maintenance window ended at %v, using default", w.End)
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

// GetMaintenanceWindow returns the STORED window verbatim (nil if none is scheduled), for admin tooling
// that needs to show what is set -- distinct from MaintenanceWindowFor, which resolves the value actually
// served (falling back to a default future window when nothing is stored or the stored one has elapsed).
func (r *WorldRepository) GetMaintenanceWindow(ctx context.Context) (*MaintenanceWindow, error) {
	if r == nil || r.maintenance == nil {
		return nil, nil
	}
	var w MaintenanceWindow
	err := r.maintenance.FindOne(ctx, bson.M{"_id": maintenanceDocID}).Decode(&w)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// MaintenanceState classifies a (now, start, end) window into the client-visible state it drives, so admin
// tooling can report what the title will actually do. Mirrors the flag table in this file's doc comment.
type MaintenanceState int

const (
	MaintNone       MaintenanceState = iota // no coherent window (start>=end): treated as nothing
	MaintFuture                             // dated announce once per session; server AVAILABLE
	MaintCountdown                          // within the ~15 min pre-window: countdown popup
	MaintInProgress                         // now inside the window: server shows in-maintenance, OFFLINE
	MaintEnded                              // window elapsed: client treats server as OFFLINE
)

// maintenanceAnnounceLeadSeconds is the client's earliest countdown threshold (dword_82059EEC = 900s); a
// window starting within it triggers the countdown popup instead of the dated announce.
const maintenanceAnnounceLeadSeconds = 900

func (s MaintenanceState) String() string {
	switch s {
	case MaintFuture:
		return "future (dated announce once/session; server AVAILABLE)"
	case MaintCountdown:
		return "countdown (<15min; countdown popup; AVAILABLE)"
	case MaintInProgress:
		return "in-progress (server shows in-maintenance; OFFLINE)"
	case MaintEnded:
		return "ended (client treats server as OFFLINE)"
	}
	return "none"
}

// ClassifyMaintenanceWindow returns the client state a window produces at time now.
func ClassifyMaintenanceWindow(now, start, end time.Time) MaintenanceState {
	if start.IsZero() || end.IsZero() || !end.After(start) {
		return MaintNone
	}
	switch {
	case !end.After(now):
		return MaintEnded
	case !start.After(now):
		return MaintInProgress
	case start.Sub(now) <= maintenanceAnnounceLeadSeconds*time.Second:
		return MaintCountdown
	default:
		return MaintFuture
	}
}
