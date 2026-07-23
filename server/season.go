package server

import (
	"ChromehoundsStatusServer/persistence"
	"context"
	"errors"
	"fmt"
	"sync"

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
	ID     string `bson:"_id"`
	Season int    `bson:"season"`
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
