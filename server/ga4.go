package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

type GA4Client struct {
	measurementID string
	apiSecret     string
	timeout       time.Duration
	async         bool
	httpClient    *http.Client
}

func newGA4ClientFromConfig(cfg Config) *GA4Client {
	if cfg.GAMeasurementID == "" || cfg.GAAPISecret == "" {
		return nil
	}
	timeout := cfg.GATimeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &GA4Client{
		measurementID: cfg.GAMeasurementID,
		apiSecret:     cfg.GAAPISecret,
		timeout:       timeout,
		async:         cfg.GAAsync,
		httpClient:    &http.Client{Timeout: timeout},
	}
}

func (g *GA4Client) SendEvent(r *http.Request, eventName string, params map[string]any) {
	if g == nil || g.measurementID == "" || g.apiSecret == "" {
		return
	}
	if eventName == "" {
		eventName = "page_view"
	}

	safeParams := map[string]any{}
	for k, v := range params {
		safeParams[k] = v
	}

	clientID := extractGAClientID(r)
	ipOverride := resolveClientIP(r)
	userAgent := r.UserAgent()

	payload := map[string]any{
		"client_id": clientID,
		"events": []map[string]any{{
			"name":   eventName,
			"params": safeParams,
		}},
	}

	dispatch := func() {
		g.doSend(payload, ipOverride, userAgent)
	}

	if g.async {
		go dispatch()
		return
	}
	dispatch()
}

func (g *GA4Client) doSend(payload map[string]any, ipOverride, userAgent string) {
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("ga4 marshal payload failed: %v", err)
		return
	}

	q := url.Values{}
	q.Set("measurement_id", g.measurementID)
	q.Set("api_secret", g.apiSecret)
	if ipOverride != "" {
		q.Set("ip_override", ipOverride)
	}
	if userAgent != "" {
		q.Set("ua", userAgent)
	}

	ctx, cancel := context.WithTimeout(context.Background(), g.timeout)
	defer cancel()

	endpoint := "https://www.google-analytics.com/mp/collect?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		log.Printf("ga4 build request failed: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}

	resp, err := g.httpClient.Do(req)
	if err != nil {
		log.Printf("ga4 request failed: %v", err)
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	if resp.StatusCode >= 400 {
		log.Printf("ga4 responded with status %d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
}

func gaSendEvent(gaClient *GA4Client, r *http.Request, pageTitle, pageLocation, source string) {
	if gaClient == nil {
		return
	}
	params := map[string]any{
		"page_title":    pageTitle,
		"page_location": pageLocation,
		"source":        source,
	}
	gaClient.SendEvent(r, "page_view", params)
}

func extractGAClientID(r *http.Request) string {
	if cookie, err := r.Cookie("_ga"); err == nil {
		val := strings.TrimSpace(cookie.Value)
		if strings.HasPrefix(val, "GA") {
			parts := strings.Split(val, ".")
			if len(parts) > 2 {
				val = strings.Join(parts[2:], ".")
			}
		}
		if val != "" {
			return val
		}
	}
	return uuid.NewString()
}

func resolveClientIP(r *http.Request) string {
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		parts := strings.Split(xff, ",")
		if len(parts) > 0 {
			candidate := strings.TrimSpace(parts[0])
			if candidate != "" {
				return candidate
			}
		}
	}

	if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cf != "" {
		return cf
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}

	return strings.TrimSpace(r.RemoteAddr)
}

func currentScheme(r *http.Request) string {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		parts := strings.Split(proto, ",")
		if len(parts) > 0 {
			val := strings.TrimSpace(parts[0])
			if val != "" {
				return val
			}
		}
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func fullShortURL(r *http.Request) string {
	scheme := currentScheme(r)
	host := r.Host
	path := r.URL.Path
	if path == "" {
		path = "/"
	}
	return fmt.Sprintf("%s://%s%s", scheme, host, path)
}
