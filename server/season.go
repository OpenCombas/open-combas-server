package server

import (
	"ChromehoundsStatusServer/persistence"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The server-wide WAR SEASON number. It is the single source of truth that:
//   - keys the squad-stats aggregation buckets (currentSeason, StatBucket.BySeason);
//   - is the Status reply's "Season ID" (gameSeasonID); and
//   - is the World reply's SeasonID.
//
// It is set by the reset tool ("new season") and loaded from the DB into this process at startup (main), so a
// reset applies on the next restart -- which is exactly when a new season begins. Distinct from seasonID
// ("0001"), the fixed id-FORMAT season baked into TeamID/UserID.
const (
	serverConfigCollection = "serverConfig"
	seasonDocID            = "season"
	defaultSeasonNumber    = 14 // preserves the historical stats bucket "0014" when nothing is stored
)

var (
	seasonMu           sync.RWMutex
	serverSeasonNumber = defaultSeasonNumber
	// serverSeasonStartsAt is the unix time the current season becomes active. Until then the season is in
	// its between-seasons window: ALL maps are locked for deployment and squad leaders may change allegiance.
	// 0 / past = active (no lockout). Set by the reset tool (-lockout-window), loaded at startup by main.
	serverSeasonStartsAt int64
)

// SeasonKey formats a season number as the 4-digit id used for the stats bucket key and the wire Season IDs
// (e.g. 14 -> "0014").
func SeasonKey(n int) string { return fmt.Sprintf("%04d", n) }

// SeasonNumber returns the current server-wide war season number.
func SeasonNumber() int {
	seasonMu.RLock()
	defer seasonMu.RUnlock()
	return serverSeasonNumber
}

// ApplySeasonNumber sets the in-process season number (and keeps currentSeason, the stats bucket key, in
// step). Called by main at startup after loading from the DB.
func ApplySeasonNumber(n int) {
	if n <= 0 {
		n = defaultSeasonNumber
	}
	seasonMu.Lock()
	serverSeasonNumber = n
	seasonMu.Unlock()
	currentSeason = SeasonKey(n)
}

type seasonDoc struct {
	ID             string `bson:"_id"`
	Season         int    `bson:"season"`
	SeasonStartsAt int64  `bson:"seasonStartsAt,omitempty"` // unix; the season goes live at this time
}

// SeasonStartsAt returns the unix time the current season becomes active (0 = active now / no lockout).
func SeasonStartsAt() int64 {
	seasonMu.RLock()
	defer seasonMu.RUnlock()
	return serverSeasonStartsAt
}

// ApplySeasonStart sets the in-process season-start time (main, at startup).
func ApplySeasonStart(unix int64) {
	seasonMu.Lock()
	serverSeasonStartsAt = unix
	seasonMu.Unlock()
}

// SeasonLocked reports whether the season has not yet started, i.e. every map is currently locked for
// deployment (the between-seasons allegiance-change window). It reads the clock at call time, so the lock
// AUTO-EXPIRES the moment the start time passes -- no restart needed to open the war.
func SeasonLocked() bool { return SeasonStartsAt() > time.Now().Unix() }

// LoadSeasonStart reads the stored season-start time (0 when unset).
func LoadSeasonStart(ctx context.Context, store *persistence.Store) (int64, error) {
	var d seasonDoc
	err := store.Collection(serverConfigCollection).FindOne(ctx, bson.M{"_id": seasonDocID}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return d.SeasonStartsAt, nil
}

// SaveSeasonStart stores the season-start time (reset tool). unix<=0 clears the lockout (season active now).
func SaveSeasonStart(ctx context.Context, store *persistence.Store, unix int64) error {
	if unix < 0 {
		unix = 0
	}
	_, err := store.Collection(serverConfigCollection).UpdateOne(ctx,
		bson.M{"_id": seasonDocID},
		bson.M{"$set": bson.M{"seasonStartsAt": unix}},
		options.UpdateOne().SetUpsert(true))
	return err
}

// LoadSeasonNumber reads the stored season number, defaulting to defaultSeasonNumber when unset/invalid.
func LoadSeasonNumber(ctx context.Context, store *persistence.Store) (int, error) {
	var d seasonDoc
	err := store.Collection(serverConfigCollection).FindOne(ctx, bson.M{"_id": seasonDocID}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return defaultSeasonNumber, nil
	}
	if err != nil {
		return defaultSeasonNumber, err
	}
	if d.Season <= 0 {
		return defaultSeasonNumber, nil
	}
	return d.Season, nil
}

// SaveSeasonNumber stores the server-wide season number (reset / admin tooling). Positive values only.
func SaveSeasonNumber(ctx context.Context, store *persistence.Store, n int) error {
	if n <= 0 {
		return errors.New("season must be positive")
	}
	_, err := store.Collection(serverConfigCollection).UpdateOne(ctx,
		bson.M{"_id": seasonDocID},
		bson.M{"$set": bson.M{"season": n}},
		options.UpdateOne().SetUpsert(true))
	return err
}
