// Package persistence holds the shared MongoDB connection used to persist ChromeHounds war state
// (squads, area/battlefield occupation, battle reports, news). It connects to the SAME MongoDB
// instance as the Xenia-WebServices HTTP backend so the two services share a single source of truth;
// Xenia owns the ephemeral sessions/players/leaderboards collections, while this server owns the
// persistent war-state collections. See project_combas_server_protocol memory.
//
// Phase 0 (this file): connection plumbing only. Later phases add repositories per collection and wire
// them into the message servers.
package persistence

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// connectTimeout bounds both server selection and the initial ping so a missing/unreachable MongoDB
// fails fast at startup instead of hanging the process.
const connectTimeout = 10 * time.Second

// Store is a thin wrapper around a connected Mongo client + the target database.
type Store struct {
	client *mongo.Client
	db     *mongo.Database
}

// Connect dials the MongoDB at uri and verifies the connection with a ping. dbName must match the
// database Xenia-WebServices writes to (its Mongoose URI has no path, so it defaults to "test").
func Connect(ctx context.Context, uri, dbName string) (*Store, error) {
	if dbName == "" {
		dbName = "test"
	}

	opts := options.Client().ApplyURI(uri).SetServerSelectionTimeout(connectTimeout)
	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, fmt.Errorf("mongo connect: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := client.Ping(pingCtx, nil); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, fmt.Errorf("mongo ping: %w", err)
	}

	return &Store{client: client, db: client.Database(dbName)}, nil
}

// DB returns the target database handle.
func (s *Store) DB() *mongo.Database { return s.db }

// Collection returns a handle to a named collection in the target database.
func (s *Store) Collection(name string) *mongo.Collection { return s.db.Collection(name) }

// Close disconnects the underlying client.
func (s *Store) Close(ctx context.Context) error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.Disconnect(ctx)
}
