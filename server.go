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
	      ENV & GLOBAL METRICS
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
	ttl   time.Duration
	key   string
	httpc *http.Client
}

func newJWKSCache(jwksURL, authKey string, ttl time.Duration) *JWKSCache {
	return &JWKSCache{
		keys:   map[string]jwk.Key{},
		expiry: time.Now(), // force initial fetch
		url:    jwksURL,
		key:    authKey,
		ttl:    ttl,
		httpc:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *JWKSCache) GetKey(kid string) (jwk.Key, error) {
	c.mu.RLock()
	key, found := c.keys[kid]
	expired := time.Now().After(c.expiry)
	c.mu.RUnlock()

	if found && !expired {
		return key, nil
	}
	return c.refresh(kid)
}

func (c *JWKSCache) refresh(targetKid string) (jwk.Key, error) {
	c.refreshLock.Lock()
	defer c.refreshLock.Unlock()

	/* check again after lock */
	c.mu.RLock()
	if !time.Now().After(c.expiry) {
		k, ok := c.keys[targetKid]
		c.mu.RUnlock()
		if ok {
			return k, nil
		}
	}
	c.mu.RUnlock()

	req, err := http.NewRequest("GET", c.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("apikey", c.key)
	req.Header.Set("Authorization", "Bearer "+c.key)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("jwks http %d: %s", resp.StatusCode, body)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	set, err := jwk.Parse(raw)
	if err != nil {
		return nil, err
	}

	tmp := make(map[string]jwk.Key)
	var found jwk.Key
	for it := set.Keys(context.Background()); it.Next(context.Background()); {
		pair := it.Pair()
		k := pair.Value.(jwk.Key)
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

	return found, nil
}

/*
	───────────────────────────────
	       JWT VALIDATION
	───────────────────────────────
*/

func extractHeaderKidAlg(tok string) (kid, alg string, err error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return "", "", errors.New("token must have 3 parts")
	}
	hdrRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", "", err
	}
	var hdr map[string]any
	if err := json.Unmarshal(hdrRaw, &hdr); err != nil {
		return "", "", err
	}
	if v, ok := hdr["kid"].(string); ok {
		kid = v
	}
	if v, ok := hdr["alg"].(string); ok {
		alg = v
	}
	return
}

func validateToken(tokenStr string) (string, error) {
	kid, hdrAlg, err := extractHeaderKidAlg(tokenStr)
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

	tok, err := jwt.Parse([]byte(tokenStr),
		jwt.WithKey(key.Algorithm(), key),
		jwt.WithValidate(true),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired()) {
			return "", fmt.Errorf("token expired: %w", err)
		}
		return "", fmt.Errorf("token invalid: %w", err)
	}

	if hdrAlg != "" && hdrAlg != key.Algorithm().String() && key.Algorithm() != jwa.NoSignature {
		slog.Warn("alg header mismatch", "kid", kid, "hdr_alg", hdrAlg, "key_alg", key.Algorithm())
	}

	sub := tok.Subject()
	if sub == "" {
		return "", errors.New("subject claim missing")
	}
	return sub, nil
}

/*
	───────────────────────────────
	      WEBSOCKET SERVER
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
	c := &Client{conn: ws, userID: uid, projectID: pid}
	r.mu.Lock()
	r.clients[uid] = c
	r.lastActivity = time.Now()
	r.mu.Unlock()
	usersCounter.Inc()
	return c
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
	       APPLICATION MAIN
	───────────────────────────────
*/

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{AddSource: false})))

	if supabaseURL == "" {
		slog.Error("SUPABASE_URL not set")
		os.Exit(1)
	}
	// choose key: service > anon
	key := supabaseServiceKey
	if key == "" {
		key = supabaseAnonKey
	}
	if key == "" {
		slog.Error("SUPABASE_ANON_KEY or SUPABASE_SERVICE_KEY must be set")
		os.Exit(1)
	}

	jwksURL := fmt.Sprintf("%s/auth/v1/keys?apikey=%s", supabaseURL, url.QueryEscape(key))
	jwksCache = newJWKSCache(jwksURL, key, 15*time.Minute)

	prometheus.MustRegister(usersCounter, roomsCounter)

	e := echo.New()
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	server := NewServer()

	e.GET("/status", func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
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
			if proto := c.Request().Header.Get("Sec-WebSocket-Protocol"); proto != "" {
				parts := strings.Split(proto, ",")
				if len(parts) == 2 && strings.TrimSpace(parts[0]) == "Bearer" {
					auth = "Bearer " + strings.TrimSpace(parts[1])
				}
			}
		}
		if auth == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "Authorization required")
		}
		p := strings.SplitN(auth, " ", 2)
		if len(p) != 2 || !strings.EqualFold(p[0], "bearer") {
			return echo.NewHTTPError(http.StatusBadRequest, "Auth format: Bearer <token>")
		}

		sub, err := validateToken(p[1])
		if err != nil {
			return echo.NewHTTPError(http.StatusUnauthorized, err.Error())
		}
		if sub != uid {
			return echo.NewHTTPError(http.StatusUnauthorized, "token/user mismatch")
		}

		ws, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
		if err != nil {
			return err
		}

		client := server.add(pid, uid, ws)
		defer server.remove(client)

		for {
			mt, msg, err := ws.ReadMessage()
			if err != nil {
				break
			}
			server.broadcast(pid, uid, mt, msg)
		}
		return nil
	})

	if err := e.Start(":8989"); err != nil {
		slog.Error("echo start", "error", err)
	}
}
