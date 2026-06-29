package persistence

import (
	"context"
	"os"
	"testing"
)

// TestConnectInvalidURI checks the error path without needing a server: a non-mongo scheme is rejected
// by the driver at Connect time (no network, no timeout wait).
func TestConnectInvalidURI(t *testing.T) {
	if _, err := Connect(context.Background(), "http://not-a-mongo-uri", "test"); err == nil {
		t.Fatal("expected error for invalid mongo URI, got nil")
	}
}

// TestConnectLive exercises the happy path against a real MongoDB. It is skipped unless MONGO_TEST_URI
// is set, so the suite stays green on machines without a database.
func TestConnectLive(t *testing.T) {
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set MONGO_TEST_URI (e.g. mongodb://127.0.0.1:27017/) to run the live MongoDB test")
	}

	store, err := Connect(context.Background(), uri, "test")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer store.Close(context.Background())

	if store.DB() == nil {
		t.Fatal("expected non-nil database handle")
	}
	if store.Collection("squads") == nil {
		t.Error("expected non-nil collection handle")
	}
}

// TestCloseNil ensures Close on a zero/nil Store is a safe no-op (the main loop may never assign one).
func TestCloseNil(t *testing.T) {
	var s *Store
	if err := s.Close(context.Background()); err != nil {
		t.Errorf("Close on nil Store: %v", err)
	}
}
