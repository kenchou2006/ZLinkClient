package server

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

var (
	clientsOnce sync.Once
	clientsDB   *sql.DB
	clientsRDB  *redis.Client
	clientsErr  error
)

// InitClientsOnce reads env and attempts to initialize DB and Redis clients once.
// It returns the initialized *sql.DB and *redis.Client (either may be nil) and an error
// describing the first encountered failure (if any). It is safe to call from multiple goroutines.
func InitClientsOnce() (*sql.DB, *redis.Client, error) {
	clientsOnce.Do(func() {
		_ = godotenv.Load()
		// Postgres
		pg := os.Getenv("POSTGRES_URL")
		if pg == "" {
			pg = os.Getenv("DATABASE_URL")
		}
		if pg != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			db, err := sql.Open("postgres", pg)
			if err != nil {
				clientsErr = err
				return
			}
			if err := db.PingContext(ctx); err != nil {
				db.Close()
				clientsErr = err
				return
			}
			// sensible defaults
			db.SetMaxOpenConns(10)
			db.SetMaxIdleConns(5)
			clientsDB = db
		}

		// Redis
		rurl := os.Getenv("REDIS_URL")
		raddr := os.Getenv("REDIS_ADDR")
		if rurl != "" {
			opt, err := redis.ParseURL(rurl)
			if err != nil {
				clientsErr = err
				return
			}
			r := redis.NewClient(opt)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := r.Ping(ctx).Err(); err != nil {
				clientsErr = err
				return
			}
			clientsRDB = r
		} else if raddr != "" {
			r := redis.NewClient(&redis.Options{Addr: raddr})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := r.Ping(ctx).Err(); err != nil {
				clientsErr = err
				return
			}
			clientsRDB = r
		}
	})
	return clientsDB, clientsRDB, clientsErr
}

// CloseClients closes clients if they were initialized.
func CloseClients() {
	if clientsDB != nil {
		_ = clientsDB.Close()
		clientsDB = nil
	}
	if clientsRDB != nil {
		_ = clientsRDB.Close()
		clientsRDB = nil
	}
}
