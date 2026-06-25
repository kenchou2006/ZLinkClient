package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	tpl "ZLinkClient/templates"

	"github.com/redis/go-redis/v9"
)

var safeIdent = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)
var effectiveCacheKeyPrefix string

func validateIdentifier(name string) error {
	if !safeIdent.MatchString(name) {
		return fmt.Errorf("invalid identifier: %s", name)
	}
	return nil
}

func linkCacheKey(code string) string {
	if effectiveCacheKeyPrefix != "" {
		return effectiveCacheKeyPrefix + code
	}
	return "link:" + code
}

func getCacheKeyVariants(code string) []string {
	variants := []string{}
	add := func(k string) {
		if k == "" {
			return
		}
		for _, v := range variants {
			if v == k {
				return
			}
		}
		variants = append(variants, k)
	}
	add(linkCacheKey(code))
	add(linkCacheKey(url.QueryEscape(code)))
	noAt := strings.TrimPrefix(code, "@")
	if noAt != code {
		add(linkCacheKey(noAt))
		add(linkCacheKey(url.QueryEscape(noAt)))
	}
	return variants
}

type linkCacheValue struct {
	URL      string  `json:"url"`
	ID       int64   `json:"id"`
	CachedAt float64 `json:"cached_at"`
	// Unix seconds; 0 means the link never expires.
	ExpiresAt int64 `json:"expires_at,omitempty"`
}

// expired reports whether a cached link has passed its expiry time.
func (c linkCacheValue) expired() bool {
	return c.ExpiresAt != 0 && time.Now().Unix() >= c.ExpiresAt
}

func getOriginalURL(ctx context.Context, db *sql.DB, tableName, codeCol, urlCol, code string) (int64, string, sql.NullTime, error) {
	var expiresAt sql.NullTime
	if err := validateIdentifier(tableName); err != nil {
		return 0, "", expiresAt, err
	}
	if err := validateIdentifier(codeCol); err != nil {
		return 0, "", expiresAt, err
	}
	if err := validateIdentifier(urlCol); err != nil {
		return 0, "", expiresAt, err
	}
	// expires_at is a fixed Django field; NULL means the link never expires.
	query := fmt.Sprintf("SELECT id, %s, expires_at FROM %s WHERE %s = $1 LIMIT 1", urlCol, tableName, codeCol)
	var id int64
	var original string
	err := db.QueryRowContext(ctx, query, code).Scan(&id, &original, &expiresAt)
	return id, original, expiresAt, err
}

// Config holds runtime configuration for the router.
type Config struct {
	TableName       string
	CodeColumn      string
	URLColumn       string
	CacheKeyPrefix  string
	CacheVersion    string
	CachePersistent bool
	CacheTTL        time.Duration
	GAMeasurementID string
	GAAPISecret     string
	GATimeout       time.Duration
	GAAsync         bool
}

// LoadConfigFromEnv reads environment variables and returns Config with defaults.
func LoadConfigFromEnv() Config {
	// table/column names are fixed by design; not configurable via env.
	// If you need to change them, update the constants here.
	tableName := "shortener_link"
	codeCol := "short_code"
	urlCol := "original_url"

	// CACHE_KEY_PREFIX is fixed by design; not configurable via env.
	// If you need to change it, update the constant here.
	cacheKeyPrefix := ":shortener:url:"
	// default CACHE_VERSION to "0" when not set
	cacheVersion := os.Getenv("CACHE_VERSION")
	if cacheVersion == "" {
		cacheVersion = "0"
	}
	// CACHE_TTL parsing
	cacheTTLEnv := os.Getenv("CACHE_TTL")
	if cacheTTLEnv == "" {
		cacheTTLEnv = os.Getenv("CACHE_TTL_SECONDS")
	}
	cacheTTLEnv = strings.TrimSpace(cacheTTLEnv)
	cacheTTLsec := -1
	if cacheTTLEnv != "" {
		if v, err := strconv.Atoi(cacheTTLEnv); err == nil {
			cacheTTLsec = v
		}
	}
	cachePersistent := cacheTTLsec < 0
	var cacheTTL time.Duration
	if cachePersistent {
		cacheTTL = 0
	} else {
		cacheTTL = time.Duration(cacheTTLsec) * time.Second
	}

	gaMeasurementID := strings.TrimSpace(os.Getenv("GA_MEASUREMENT_ID"))
	gaAPISecret := strings.TrimSpace(os.Getenv("GA_API_SECRET"))

	gaTimeout := 3 * time.Second
	if gaTimeoutEnv := strings.TrimSpace(os.Getenv("GA4_TIMEOUT")); gaTimeoutEnv != "" {
		if v, err := strconv.Atoi(gaTimeoutEnv); err == nil && v > 0 {
			gaTimeout = time.Duration(v) * time.Second
		}
	}
	gaAsync := true
	if gaAsyncEnv := strings.TrimSpace(os.Getenv("GA4_ASYNC")); gaAsyncEnv != "" {
		if v, err := strconv.ParseBool(gaAsyncEnv); err == nil {
			gaAsync = v
		}
	}

	return Config{
		TableName:       tableName,
		CodeColumn:      codeCol,
		URLColumn:       urlCol,
		CacheKeyPrefix:  cacheKeyPrefix,
		CacheVersion:    cacheVersion,
		CachePersistent: cachePersistent,
		CacheTTL:        cacheTTL,
		GAMeasurementID: gaMeasurementID,
		GAAPISecret:     gaAPISecret,
		GATimeout:       gaTimeout,
		GAAsync:         gaAsync,
	}
}

// NewRouterFromConfig convenience wrapper that builds the router from a Config.
func NewRouterFromConfig(db *sql.DB, redisClient *redis.Client, cfg Config) http.Handler {
	return NewRouter(db, redisClient, cfg.TableName, cfg.CodeColumn, cfg.URLColumn, cfg.CacheKeyPrefix, cfg.CacheVersion, cfg.CachePersistent, cfg.CacheTTL, newGA4ClientFromConfig(cfg))
}

// NewRouter builds an http.Handler with routes wired. Pass nil db/redisClient for serverless minimal mode.
func NewRouter(db *sql.DB, redisClient *redis.Client, tableName, codeCol, urlCol, cacheKeyPrefix, cacheVersion string, cachePersistent bool, cacheTTL time.Duration, gaClient *GA4Client) http.Handler {
	// compute effective prefix robustly
	if cacheVersion != "" {
		trimmed := strings.Trim(cacheKeyPrefix, ":")
		if trimmed != "" {
			effectiveCacheKeyPrefix = ":" + cacheVersion + ":" + trimmed + ":"
		} else {
			effectiveCacheKeyPrefix = ":" + cacheVersion + ":"
		}
	} else {
		// no version provided; use provided prefix if any, ensure it ends with ':'
		if cacheKeyPrefix != "" {
			if strings.HasSuffix(cacheKeyPrefix, ":") {
				effectiveCacheKeyPrefix = cacheKeyPrefix
			} else {
				effectiveCacheKeyPrefix = cacheKeyPrefix + ":"
			}
		} else {
			// fallback to simple 'link:' prefix
			effectiveCacheKeyPrefix = "link:"
		}
	}

	mux := http.NewServeMux()

	// healthz
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// (optional) static file server under /static/ if you have a static folder
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	// To support dynamic single-segment codes like /:code, use a top-level handler
	top := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		// If it's a known static route, let the mux handle it
		if strings.HasPrefix(p, "/healthz") || strings.HasPrefix(p, "/static/") {
			mux.ServeHTTP(w, r)
			return
		}

		// Root path handling
		if p == "/" || p == "" {
			rootCode := "@root"
			variants := getCacheKeyVariants(rootCode)

			if redisClient != nil {
				for _, k := range variants {
					val, err := redisClient.Get(r.Context(), k).Result()
					if err == nil && val != "" {
						var cached linkCacheValue
						jerr := json.Unmarshal([]byte(val), &cached)
						if jerr == nil {
							if cached.expired() {
								// Stale/expired: drop the key and fall through to DB (which will 404).
								redisClient.Del(r.Context(), k)
								break
							}
							if !cachePersistent {
								if err := redisClient.Expire(r.Context(), k, cacheTTL).Err(); err != nil {
									log.Printf("Cache touch failed for key %s: %v", k, err)
								}
							}
							// cache hit: report GA with source=cache
							gaSendEvent(gaClient, r, cached.URL, fullShortURL(r), "cache")
							http.Redirect(w, r, cached.URL, http.StatusFound)
							return
						}
						// invalid format: treat as cache miss (do not return), log and continue to DB
						log.Printf("invalid cache format for key %s: %v", k, jerr)
					}
					if err != nil && err != redis.Nil {
						log.Printf("Cache get failed for key %s: %v", k, err)
					}
				}
			}

			if db == nil {
				render404(w, r)
				return
			}
			id, orig, expiresAt, err := getOriginalURL(r.Context(), db, tableName, codeCol, urlCol, rootCode)
			if err == nil {
				if expiresAt.Valid && !expiresAt.Time.After(time.Now()) {
					render404(w, r)
					return
				}
				if redisClient != nil {
					cached := linkCacheValue{URL: orig, ID: id, CachedAt: float64(time.Now().Unix())}
					if expiresAt.Valid {
						cached.ExpiresAt = expiresAt.Time.Unix()
					}
					b, jerr := json.Marshal(cached)
					if jerr == nil {
						canonical := variants[0]
						if err := redisClient.Set(r.Context(), canonical, b, cacheTTL).Err(); err != nil {
							log.Printf("Cache set failed for root key %s: %v", canonical, err)
						}
					} else {
						log.Printf("Cache marshal failed for root key %s: %v", rootCode, jerr)
					}
				}
				gaSendEvent(gaClient, r, orig, fullShortURL(r), "db")
				http.Redirect(w, r, orig, http.StatusFound)
				return
			}
			if err == sql.ErrNoRows {
				render404(w, r)
				return
			}
			log.Printf("db error when resolving @root: %v", err)
			render404(w, r)
			return
		}

		// handle single-segment code like /abc
		// strip leading '/'
		code := strings.TrimPrefix(p, "/")
		if code == "" {
			render404(w, r)
			return
		}

		variants := getCacheKeyVariants(code)

		if redisClient != nil {
			for _, k := range variants {
				val, err := redisClient.Get(r.Context(), k).Result()
				if err == nil && val != "" {
					var cached linkCacheValue
					jerr := json.Unmarshal([]byte(val), &cached)
					if jerr == nil {
						if cached.expired() {
							// Stale/expired: drop the key and fall through to DB (which will 404).
							redisClient.Del(r.Context(), k)
							break
						}
						if !cachePersistent {
							if err := redisClient.Expire(r.Context(), k, cacheTTL).Err(); err != nil {
								log.Printf("Cache touch failed for key %s: %v", k, err)
							}
						}
						// cache hit: report GA with source=cache
						gaSendEvent(gaClient, r, cached.URL, fullShortURL(r), "cache")
						http.Redirect(w, r, cached.URL, http.StatusFound)
						return
					}
					// invalid format: treat as cache miss (do not return), log and continue to DB
					log.Printf("invalid cache format for key %s: %v", k, jerr)
				}
				if err != nil && err != redis.Nil {
					log.Printf("Cache get failed for key %s: %v", k, err)
				}
			}
		}

		if db == nil {
			render404(w, r)
			return
		}
		id, orig, expiresAt, err := getOriginalURL(r.Context(), db, tableName, codeCol, urlCol, code)
		if err != nil {
			if err == sql.ErrNoRows {
				render404(w, r)
				return
			}
			log.Printf("db error: %v", err)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"internal"}`))
			return
		}

		// Expired links are treated as not found.
		if expiresAt.Valid && !expiresAt.Time.After(time.Now()) {
			render404(w, r)
			return
		}

		if redisClient != nil {
			cached := linkCacheValue{URL: orig, ID: id, CachedAt: float64(time.Now().Unix())}
			if expiresAt.Valid {
				cached.ExpiresAt = expiresAt.Time.Unix()
			}
			b, jerr := json.Marshal(cached)
			if jerr == nil {
				canonical := variants[0]
				if err := redisClient.Set(r.Context(), canonical, b, cacheTTL).Err(); err != nil {
					log.Printf("Cache set failed for key %s: %v", canonical, err)
				}
			} else {
				log.Printf("Cache marshal failed for key %s: %v", code, jerr)
			}
		} else {
			log.Printf("redis client is nil; skipping cache set for key %s", code)
		}

		gaSendEvent(gaClient, r, orig, fullShortURL(r), "db")
		http.Redirect(w, r, orig, http.StatusFound)
	})

	return top
}

var (
	cachedRouter http.Handler
	cacheOnce    sync.Once
)

// resolve404Path attempts to find the 404 template relative to the working dir or the executable.
func resolve404Path() string {
	candidates := []string{"templates/404.html"}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates, filepath.Join(dir, "templates", "404.html"))
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "templates", "404.html"))
	}
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

func render404(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	if len(tpl.NotFound) > 0 {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(tpl.NotFound)
		return
	}
	if path := resolve404Path(); path != "" {
		http.ServeFile(w, r, path)
		return
	}
	_, _ = w.Write([]byte("404 page not found"))
}

func GetOrBuildRouter(db *sql.DB, redisClient *redis.Client, cfg Config) http.Handler {
	cacheOnce.Do(func() {
		cachedRouter = NewRouterFromConfig(db, redisClient, cfg)
	})
	return cachedRouter
}
