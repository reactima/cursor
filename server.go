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
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

/*
	───────────────────────────────
	   ENVIRONMENT / GLOBAL VARS
	───────────────────────────────
*/

var (
	/* prometheus */
	usersCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "connected_users_total", Help: "Total websocket users ever connected",
	})
	roomsCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rooms_total", Help: "Total project rooms ever created",
	})

	/* Supabase */
	supabaseURL     = strings.TrimRight(os.Getenv("SUPABASE_URL"), "/")
	supabaseAnonKey = os.Getenv("SUPABASE_ANON_KEY")

	/* JWKS cache singleton */
	jwksCache *JWKSCache
)

/*
	───────────────────────────────
	   JWKS CACHE (singleton)
	───────────────────────────────
*/

type JWKSCache struct {
	keys        map[string]jwk.Key
	expiresAt   time.Time
	mu          sync.RWMutex
	refreshLock sync.Mutex

	jwksURL string
	anonKey string
	ttl     time.Duration
	client  *http.Client
}

func newJWKSCache(url, anon string, ttl time.Duration) *JWKSCache {
	return &JWKSCache{
		keys:      map[string]jwk.Key{},
		expiresAt: time.Now(), // force initial refresh
		jwksURL:   url,
		anonKey:   anon,
		ttl:       ttl,
		client:    &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *JWKSCache) GetKey(kid string) (jwk.Key, error) {
	c.mu.RLock()
	key, ok := c.keys[kid]
	expired := time.Now().After(c.expiresAt)
	c.mu.RUnlock()

	if ok && !expired {
		return key, nil
	}

	return c.refresh(kid)
}

func (c *JWKSCache) refresh(targetKid string) (jwk.Key, error) {
	c.refreshLock.Lock()
	defer c.refreshLock.Unlock()

	/* double-check after obtaining refresh lock */
	c.mu.RLock()
	if !time.Now().After(c.expiresAt) {
		key, ok := c.keys[targetKid]
		c.mu.RUnlock()
		if ok {
			return key, nil
		}
		c.mu.RUnlock()
	}
	c.mu.RUnlock()

	req, err := http.NewRequest("GET", c.jwksURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("apikey", c.anonKey)
	req.Header.Set("Authorization", "Bearer "+c.anonKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("jwks http %d: %s", resp.StatusCode, b)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	set, err := jwk.Parse(raw)
	if err != nil {
		return nil, err
	}

	newKeys := make(map[string]jwk.Key)
	var found jwk.Key
	it := set.Keys(context.Background())
	for it.Next(context.Background()) {
		k := it.Pair().Value.(jwk.Key)
		newKeys[k.KeyID()] = k
		if k.KeyID() == targetKid {
			found = k
		}
	}

	c.mu.Lock()
	c.keys = newKeys
	c.expiresAt = time.Now().Add(c.ttl)
	c.mu.Unlock()

	return found, nil
}

/*
	───────────────────────────────
	   JWT VALIDATION HELPERS
	───────────────────────────────
*/

func extractKidAlg(token string) (kid, alg string, err error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", "", errors.New("token parts != 3")
	}
	hdr, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", "", err
	}
	var m map[string]any
	if err := json.Unmarshal(hdr, &m); err != nil {
		return "", "", err
	}
	if v, ok := m["kid"].(string); ok {
		kid = v
	}
	if v, ok := m["alg"].(string); ok {
		alg = v
	}
	return
}

func verifyToken(tokenStr string) (string, error) {
	kid, alg, err := extractKidAlg(tokenStr)
	if err != nil {
		return "", fmt.Errorf("header parse: %w", err)
	}
	if kid == "" {
		return "", errors.New("kid missing in header")
	}
	key, err := jwksCache.GetKey(kid)
	if err != nil {
		return "", err
	}

	parsed, err := jwt.Parse([]byte(tokenStr),
		jwt.WithKey(key.Algorithm(), key),
		jwt.WithValidate(true),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired()) {
			return "", fmt.Errorf("token expired: %w", err)
		}
		return "", fmt.Errorf("token invalid: %w", err)
	}

	/* optional alg mismatch warn */
	if alg != "" && alg != key.Algorithm().String() {
		slog.Warn("alg mismatch", "header_alg", alg, "key_alg", key.Algorithm(), "kid", kid)
	}

	sub := parsed.Subject()
	if sub == "" {
		return "", errors.New("subject claim missing")
	}
	return sub, nil
}

/*
	───────────────────────────────
	    WEBSOCKET SERVER MODEL
	───────────────────────────────
*/

type Client struct {
	conn        *websocket.Conn
	userUUID    string
	projectUUID string
	mu          sync.Mutex
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
	t := time.NewTicker(time.Minute)
	for range t.C {
		cut := time.Now().Add(-15 * time.Minute)
		s.mu.Lock()
		for id, room := range s.rooms {
			room.mu.RLock()
			if room.lastActivity.Before(cut) {
				delete(s.rooms, id)
			}
			room.mu.RUnlock()
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
	return r
}

func (s *Server) add(pid, uid string, ws *websocket.Conn) *Client {
	r := s.room(pid)
	c := &Client{conn: ws, userUUID: uid, projectUUID: pid}
	r.mu.Lock()
	r.clients[uid] = c
	r.lastActivity = time.Now()
	r.mu.Unlock()
	usersCounter.Inc()
	return c
}

func (s *Server) remove(c *Client) {
	s.mu.RLock()
	r, ok := s.rooms[c.projectUUID]
	s.mu.RUnlock()
	if !ok {
		return
	}
	r.mu.Lock()
	delete(r.clients, c.userUUID)
	r.lastActivity = time.Now()
	r.mu.Unlock()
	c.conn.Close()
}

func (s *Server) broadcast(pid, sender string, mt int, msg []byte) {
	s.mu.RLock()
	r, ok := s.rooms[pid]
	s.mu.RUnlock()
	if !ok {
		return
	}
	r.mu.RLock()
	targets := make([]*Client, 0, len(r.clients))
	for id, cl := range r.clients {
		if id != sender {
			targets = append(targets, cl)
		}
	}
	r.mu.RUnlock()

	for _, cl := range targets {
		cl.mu.Lock()
		_ = cl.conn.WriteMessage(mt, msg)
		cl.mu.Unlock()
	}
}

/*
	───────────────────────────────
	      APPLICATION ENTRY
	───────────────────────────────
*/

func main() {
	/* ─── sanity env ─── */
	if supabaseURL == "" || supabaseAnonKey == "" {
		slog.Error("SUPABASE_URL or SUPABASE_ANON_KEY missing")
		os.Exit(1)
	}

	/* ─── JWKS cache ─── */
	jwksCache = newJWKSCache(
		fmt.Sprintf("%s/auth/v1/jwks", supabaseURL),
		supabaseAnonKey,
		15*time.Minute,
	)

	/* ─── prometheus ─── */
	prometheus.MustRegister(usersCounter, roomsCounter)

	/* ─── echo setup ─── */
	e := echo.New()
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	srv := NewServer()

	e.GET("/status", func(c echo.Context) error {
		return c.JSON(http.StatusOK, echo.Map{
			"status": "ok",
		})
	})
	e.GET("/metrics", echo.WrapHandler(promhttp.Handler()))

	upgrader := websocket.Upgrader{
		CheckOrigin:  func(r *http.Request) bool { return true },
		Subprotocols: []string{"Bearer"},
	}

	e.GET("/ws", func(c echo.Context) error {
		uid := c.QueryParam("user_uuid")
		pid := c.QueryParam("project_uuid")
		if uid == "" || pid == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "user_uuid & project_uuid required")
		}

		auth := c.Request().Header.Get("Authorization")
		if auth == "" {
			if p := c.Request().Header.Get("Sec-WebSocket-Protocol"); p != "" {
				if parts := strings.Split(p, ","); len(parts) == 2 && strings.TrimSpace(parts[0]) == "Bearer" {
					auth = "Bearer " + strings.TrimSpace(parts[1])
				}
			}
		}
		if auth == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "authorization missing")
		}
		parts := strings.SplitN(auth, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
			return echo.NewHTTPError(http.StatusBadRequest, "bad auth format")
		}

		sub, err := verifyToken(parts[1])
		if err != nil {
			return echo.NewHTTPError(http.StatusUnauthorized, err.Error())
		}
		if sub != uid {
			return echo.NewHTTPError(http.StatusUnauthorized, "token/uid mismatch")
		}

		ws, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
		if err != nil {
			return err
		}

		cl := srv.add(pid, uid, ws)
		defer srv.remove(cl)

		for {
			mt, msg, err := ws.ReadMessage()
			if err != nil {
				break
			}
			srv.broadcast(pid, uid, mt, msg)
		}
		return nil
	})

	if err := e.Start(":8989"); err != nil {
		slog.Error("echo start", "error", err)
	}
}
