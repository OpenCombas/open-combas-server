// Command chapi is a STANDALONE, read-only HTTP API that exposes ChromeHounds-specific data (starting with
// active players by nation) from the shared combas MongoDB. It is a separate service/binary from the UDP
// combas server -- it just reuses that module's repositories so the war/squad domain isn't duplicated. Point
// a dashboard (e.g. Xenia-WebServices src/public) at it.
//
// Config: Mongo from config.toml + MONGO_URI/MONGO_DATABASE (same as the server/reset tools); listen address
// from CH_API_LISTEN (default :8099); CORS allow-origin from CH_API_CORS (default "*", read-only aggregates).
//
//	go run ./cmd/chapi                 # from the server module directory
//	curl -s localhost:8099/api/players-by-nation
package main

import (
	"ChromehoundsStatusServer/config"
	"ChromehoundsStatusServer/logging"
	"ChromehoundsStatusServer/persistence"
	"ChromehoundsStatusServer/server"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"time"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	cfg := config.LoadConfig()
	if !cfg.Mongo.Enabled {
		logging.Error.Fatalf("[CHAPI] Mongo is not enabled; set MONGO_URI (and MONGO_DATABASE) or enable [Mongo] in config.toml")
	}
	listen := envOr("CH_API_LISTEN", ":8099")
	cors := envOr("CH_API_CORS", "*")

	connectCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store, err := persistence.Connect(connectCtx, cfg.Mongo.URI, cfg.Mongo.Database)
	if err != nil {
		logging.Error.Fatalf("[CHAPI] mongo connect failed: %v", err)
	}
	squad := server.NewSquadRepository(store)

	mux := http.NewServeMux()

	// GET /api/players-by-nation -> {"online":{"A":..},"registered":{"A":..},"generatedAt":"..."}
	mux.HandleFunc("/api/players-by-nation", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", cors)
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		reqCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		counts, err := squad.PlayersByNation(reqCtx)
		if err != nil {
			logging.Warn.Printf("[CHAPI] players-by-nation failed: %v", err)
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{
			"online":      counts.Online,
			"registered":  counts.Registered,
			"generatedAt": time.Now().UTC().Format(time.RFC3339),
		})
	})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	logging.Info.Printf("[CHAPI] ChromeHounds API listening on %s (db %q) -- GET /api/players-by-nation", listen, cfg.Mongo.Database)
	if err := http.ListenAndServe(listen, mux); err != nil {
		logging.Error.Fatalf("[CHAPI] server exited: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logging.Warn.Printf("[CHAPI] encode failed: %v", err)
	}
}
