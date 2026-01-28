package main

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"ZLinkClient/server"
	"github.com/joho/godotenv"
)

func main() {
	// load .env (optional)
	_ = godotenv.Load()

	// load shared config from env (centralized)
	cfg := server.LoadConfigFromEnv()

	// initialize shared clients once
	db, rdb, err := server.InitClientsOnce()
	if err != nil {
		log.Printf("warning: some clients failed to init: %v", err)
	}
	defer server.CloseClients()

	// build router using shared clients and config
	r := server.NewRouterFromConfig(db, rdb, cfg)

	// port (not part of Config since it's runtime for the long-running server)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := fmt.Sprintf(":%s", port)
	log.Printf("starting server on %s", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
