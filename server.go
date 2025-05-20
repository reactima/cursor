// cmd/ws-server/main.go
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

/*
	───────────────────────────────
	        METRICS + ENV
	───────────────────────────────
*/

var (
	usersCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "connected_users_total", Help: "Total websocket users ever connected",
	})
	roomsCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rooms_total", Help: "Total project rooms ever created",
	})

	supabaseURL        = strings.TrimRight(os.Getenv("SUPABASE_URL"), "/")
	supabaseAnonKey    = os.Getenv("SUPABASE_ANON_KEY")
	supabaseServiceKey = os.Getenv("SUPABASE_SERVICE_KEY")

	jwksCache *JWKSCache
)

/*
	───────────────────────────────
	         JWKS CACHE
	───────────────────────────────
*/

type JWKSCache struct {
	keys        map[string]jwk.Key
	expiry      time.Time
	mu          sync.RWMutex
	refreshLock sync.Mutex

	url   string
	key   string
	ttl   time.Duration
	httpc *http.Client
}

func newJWKSCache(jwksURL, key string, ttl time.Duration) *JWKSCache {
	return &JWKSCache{
		keys:   map[string]jwk.Key{},
		expiry: time.Now(), // force first refresh
		url:    jwksURL,
		key:    key,
		ttl:    ttl,
		httpc:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *JWKSCache) GetKey(kid string) (jwk.Key, error) {
	c.mu.RLock()
	key, ok := c.keys[kid]
	expired := time.Now().After(c.expiry)
	c.mu.RUnlock()

	if ok && !expired {
		slog.Debug("JWKS cache hit", "kid", kid)
		return key, nil
	}
	return c.refresh(kid)
}

func (c *JWKSCache) refresh(targetKid string) (jwk.Key, error) {
	c.refreshLock.Lock()
	defer c.refreshLock.Unlock()

	// another goroutine may already have refreshed
	c.mu.RLock()
	if !time.Now().After(c.expiry) {
		if k, ok := c.keys[targetKid]; ok {
			c.mu.RUnlock()
			return k, nil
		}
	}
	c.mu.RUnlock()

	slog.Info("Refreshing JWKS", "url", c.url, "target_kid", targetKid)

	req, err := http.NewRequest("GET", c.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("apikey", c.key)
	req.Header.Set("Authorization", "Bearer "+c.key)

	resp, err := c.httpc.Do(req)
	if err != nil {
		slog.Error("JWKS HTTP request failed", "error", err.Error())
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		slog.Error("JWKS endpoint returned non-200", "status", resp.StatusCode, "body", string(b))
		return nil, fmt.Errorf("jwks http %d: %s", resp.StatusCode, b)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	set, err := jwk.Parse(body)
	if err != nil {
		slog.Error("Failed to parse JWKS", "error", err.Error())
		return nil, err
	}

	tmp := make(map[string]jwk.Key)
	var found jwk.Key
	for it := set.Keys(context.Background()); it.Next(context.Background()); {
		k := it.Pair().Value.(jwk.Key)
		if k.KeyID() == "" {
			continue
		}
		tmp[k.KeyID()] = k
		if k.KeyID() == targetKid {
			found = k
		}
	}

	c.mu.Lock()
	c.keys = tmp
	c.expiry = time.Now().Add(c.ttl)
	c.mu.Unlock()

	slog.Info("JWKS refreshed", "keys_cached", len(tmp))

	if found == nil && targetKid != "" {
		slog.Warn("JWKS refreshed but key not found", "kid", targetKid)
	}
	return found, nil
}

/*
	───────────────────────────────
	        TOKEN VALIDATION
	───────────────────────────────
*/

func headerKidAlg(tok string) (kid, alg string, err error) {
	p := strings.Split(tok, ".")
	if len(p) != 3 {
		return "", "", errors.New("token malformed")
	}
	hRaw, err := base64.RawURLEncoding.DecodeString(p[0])
	if err != nil {
		return "", "", err
	}
	var h map[string]any
	if err := json.Unmarshal(hRaw, &h); err != nil {
		return "", "", err
	}
	if v, ok := h["kid"].(string); ok {
		kid = v
	}
	if v, ok := h["alg"].(string); ok {
		alg = v
	}
	return
}

func validateToken(tok string) (string, error) {
	kid, hdrAlg, err := headerKidAlg(tok)
	if err != nil {
		return "", err
	}
	if kid == "" {
		return "", errors.New("kid missing")
	}

	key, err := jwksCache.GetKey(kid)
	if err != nil {
		return "", err
	}

	parsed, err := jwt.Parse([]byte(tok), jwt.WithKey(key.Algorithm(), key), jwt.WithValidate(true))
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired()) {
			slog.Warn("Token expired", "kid", kid)
			return "", fmt.Errorf("token expired: %w", err)
		}
		return "", fmt.Errorf("token invalid: %w", err)
	}

	if hdrAlg != "" && hdrAlg != key.Algorithm().String() && key.Algorithm() != jwa.NoSignature {
		slog.Warn("alg mismatch", "kid", kid, "hdr_alg", hdrAlg, "key_alg", key.Algorithm())
	}

	sub := parsed.Subject()
	if sub == "" {
		return "", errors.New("subject claim missing")
	}

	slog.Debug("Token validated", "kid", kid, "sub", sub)
	return sub, nil
}

/*
	───────────────────────────────
	       SERVER STRUCTURES
	───────────────────────────────
*/

type Client struct {
	conn      *websocket.Conn
	userID    string
	projectID string
	mu        sync.Mutex
}

type ProjectRoom struct {
	clients      map[string]*Client
	mu           sync.RWMutex
	lastActivity time.Time
}

type Server struct {
	rooms map[string]*ProjectRoom
	mu    sync.RWMutex
}

func NewServer() *Server {
	s := &Server{rooms: map[string]*ProjectRoom{}}
	go s.gc()
	return s
}

func (s *Server) gc() {
	tk := time.NewTicker(time.Minute)
	for range tk.C {
		cut := time.Now().Add(-15 * time.Minute)
		s.mu.Lock()
		for id, r := range s.rooms {
			r.mu.RLock()
			if r.lastActivity.Before(cut) {
				delete(s.rooms, id)
				slog.Info("room GC", "project", id)
			}
			r.mu.RUnlock()
		}
		s.mu.Unlock()
	}
}

func (s *Server) room(pid string) *ProjectRoom {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rooms[pid]; ok {
		return r
	}
	r := &ProjectRoom{clients: map[string]*Client{}, lastActivity: time.Now()}
	s.rooms[pid] = r
	roomsCounter.Inc()
	slog.Info("created room", "project", pid)
	return r
}

func (s *Server) add(pid, uid string, ws *websocket.Conn) *Client {
	r := s.room(pid)
	cl := &Client{conn: ws, userID: uid, projectID: pid}
	r.mu.Lock()
	r.clients[uid] = cl
	r.lastActivity = time.Now()
	r.mu.Unlock()
	usersCounter.Inc()
	slog.Info("added client", "user", uid, "project", pid)
	return cl
}

func (s *Server) remove(c *Client) {
	s.mu.RLock()
	r, ok := s.rooms[c.projectID]
	s.mu.RUnlock()
	if !ok {
		return
	}
	r.mu.Lock()
	delete(r.clients, c.userID)
	r.lastActivity = time.Now()
	r.mu.Unlock()
	c.conn.Close()
	slog.Info("removed client", "user", c.userID, "project", c.projectID)
}

func (s *Server) broadcast(pid, sender string, mt int, msg []byte) {
	s.mu.RLock()
	r, ok := s.rooms[pid]
	s.mu.RUnlock()
	if !ok {
		slog.Warn("broadcast to non-existing room", "project", pid)
		return
	}
	r.mu.RLock()
	targetCount := len(r.clients) - 1
	r.mu.RUnlock()

	if targetCount <= 0 {
		slog.Debug("no targets to broadcast", "project", pid)
		return
	}

	slog.Debug("broadcast", "project", pid, "from", sender, "targets", targetCount)

	r.mu.RLock()
	for id, cl := range r.clients {
		if id == sender {
			continue
		}
		cl.mu.Lock()
		if err := cl.conn.WriteMessage(mt, msg); err != nil {
			slog.Error("broadcast write failed", "error", err.Error(), "to_user", cl.userID, "project", pid)
		}
		cl.mu.Unlock()
	}
	r.mu.RUnlock()
}

/*
	───────────────────────────────
	         APPLICATION MAIN
	───────────────────────────────
*/

func init() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{AddSource: false})))

	if supabaseURL == "" {
		slog.Error("SUPABASE_URL not set")
	}
	if supabaseAnonKey == "" && supabaseServiceKey == "" {
		slog.Error("No Supabase key available – JWKS fetch will fail")
	} else {
		slog.Info("Supabase env OK", "url", supabaseURL)
	}

	prometheus.MustRegister(usersCounter, roomsCounter)
}

func main() {
	/* JWKS cache */
	apiKey := supabaseServiceKey
	if apiKey == "" {
		apiKey = supabaseAnonKey
	}

	jwksURL := fmt.Sprintf("%s/auth/v1/keys?apikey=%s", supabaseURL, url.QueryEscape(apiKey))
	jwksCache = newJWKSCache(jwksURL, apiKey, 15*time.Minute)

	e := echo.New()
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	server := NewServer()

	e.GET("/status", func(c echo.Context) error { return c.String(http.StatusOK, "ok") })
	e.GET("/metrics", echo.WrapHandler(promhttp.Handler()))

	upgrader := websocket.Upgrader{
		CheckOrigin:  func(r *http.Request) bool { return true },
		Subprotocols: []string{"Bearer"},
	}

	e.GET("/ws", func(c echo.Context) error {
		uid := c.QueryParam("user_uuid")
		pid := c.QueryParam("project_uuid")
		if uid == "" || pid == "" {
			slog.Error("missing query params", "uid", uid, "pid", pid)
			return echo.NewHTTPError(http.StatusBadRequest, "user_uuid and project_uuid required")
		}

		auth := c.Request().Header.Get("Authorization")
		if auth == "" {
			if proto := c.Request().Header.Get("Sec-WebSocket-Protocol"); proto != "" {
				if parts := strings.Split(proto, ","); len(parts) == 2 && strings.TrimSpace(parts[0]) == "Bearer" {
					auth = "Bearer " + strings.TrimSpace(parts[1])
				}
			}
		}
		if auth == "" {
			slog.Warn("authorization header missing")
			return echo.NewHTTPError(http.StatusBadRequest, "Authorization required")
		}
		parts := strings.SplitN(auth, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
			slog.Warn("invalid auth header", "header", auth)
			return echo.NewHTTPError(http.StatusBadRequest, "Authorization format must be Bearer {token}")
		}

		sub, err := validateToken(parts[1])
		if err != nil {
			slog.Warn("token validation error", "error", err.Error())
			return echo.NewHTTPError(http.StatusUnauthorized, err.Error())
		}
		if sub != uid {
			slog.Warn("token user mismatch", "token_sub", sub, "uid", uid)
			return echo.NewHTTPError(http.StatusUnauthorized, "token user mismatch")
		}

		ws, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
		if err != nil {
			slog.Error("websocket upgrade failed", "error", err.Error(), "uid", uid, "pid", pid)
			return err
		}
		slog.Info("websocket connected", "uid", uid, "pid", pid)

		cl := server.add(pid, uid, ws)
		defer server.remove(cl)

		for {
			mt, msg, err := ws.ReadMessage()
			if err != nil {
				slog.Warn("read fail", "error", err.Error(), "uid", uid)
				break
			}
			server.broadcast(pid, uid, mt, msg)
		}
		return nil
	})

	if err := e.Start(":8989"); err != nil {
		slog.Error("server start failed", "error", err.Error())
		os.Exit(1)
	}
}
