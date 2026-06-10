package main

import (
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/playgate/playgate-server/internal/api"
	"github.com/playgate/playgate-server/internal/auth"
	"github.com/playgate/playgate-server/internal/db"
)

func main() {
	addr := flag.String("addr", envOr("PLAYGATE_ADDR", ":8080"), "listen address")
	dbPath := flag.String("db", envOr("PLAYGATE_DB", "playgate.db"), "SQLite database path")
	keyPath := flag.String("key", envOr("PLAYGATE_KEY", "ed25519.pem"), "ed25519 private key PEM path")
	flag.Parse()

	// Open DB.
	database, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer database.Close()
	log.Printf("database: %s", *dbPath)

	// Load or generate ed25519 key.
	keys, err := auth.LoadOrGenerate(*keyPath)
	if err != nil {
		log.Fatalf("load/generate key: %v", err)
	}
	log.Printf("public key (base64): %s", keys.PublicKeyBase64())

	// Build and start HTTP server.
	srv := api.New(database, keys)
	log.Printf("listening on %s", *addr)
	if err := http.ListenAndServe(*addr, srv); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
