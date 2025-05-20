package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/MicahParks/keyfunc"
	"github.com/golang-jwt/jwt/v4"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// Prometheus metrics
	usersCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "connected_users_total",
		Help: "Total websocket users ever connected",
	})
	roomsCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rooms_total",
		Help: "Total project rooms ever created",
	})

	// Supabase configuration
	supabaseURL        = strings.TrimRight(os.Getenv("SUPABASE_URL"), "/")
	supabaseServiceKey = os.Getenv("SUPABASE_SERVICE_KEY")

	// JWKS for verifying JWTs
	jwks *keyfunc.JWKS
)

func init() {
	// structured JSON logging
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{AddSource: false})))

	// require necessary env vars
	if supabaseURL == "" {
		slog.Error("SUPABASE_URL is not set")
		os.Exit(1)
	}
	if supabaseServiceKey == "" {
		slog.Error("SUPABASE_SERVICE_KEY is not set")
		os.Exit(1)
	}
	slog.Info("Supabase env loaded", "url", supabaseURL)

	// fetch JWKS so we can verify tokens
	jwksURL := fmt.Sprintf("%s/auth/v1/.well-known/jwks.json", supabaseURL)
	var err error
	jwks, err = keyfunc.Get(jwksURL, keyfunc.Options{
		RefreshInterval: time.Hour,
		RefreshErrorHandler: func(err error) {
			slog.Error("JWKS refresh error", "error", err.Error())
		},
	})
	if err != nil {
		slog.Error("failed to fetch JWKS", "url", jwksURL, "error", err.Error())
		os.Exit(1)
	}

	// register metrics
	prometheus.MustRegister(usersCounter, roomsCounter)
}

// Client represents a single websocket connection
type Client struct {
	conn        *websocket.Conn
	userUUID    string
	projectUUID string
	mu          sync.Mutex
}

// ProjectRoom holds clients and last activity timestamp
type ProjectRoom struct {
	clients      map[string]*Client
	mu           sync.RWMutex
	lastActivity time.Time
}

// Server manages all project rooms
type Server struct {
	rooms map[string]*ProjectRoom
	mu    sync.RWMutex
}

// NewServer creates a Server and starts idle-room cleanup
func NewServer() *Server {
	s := &Server{rooms: make(map[string]*ProjectRoom)}
	go s.cleanupLoop()
	return s
}

// cleanupLoop removes rooms idle for 15m every minute
func (s *Server) cleanupLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-15 * time.Minute)
		s.mu.Lock()
		for pid, room := range s.rooms {
			room.mu.RLock()
			if room.lastActivity.Before(cutoff) {
				delete(s.rooms, pid)
				slog.Info("cleaned up room", "project", pid)
			}
			room.mu.RUnlock()
		}
		s.mu.Unlock()
	}
}

// getOrCreateRoom fetches or makes a room
func (s *Server) getOrCreateRoom(pid string) *ProjectRoom {
	s.mu.Lock()
	defer s.mu.Unlock()
	if room, ok := s.rooms[pid]; ok {
		return room
	}
	room := &ProjectRoom{
		clients:      make(map[string]*Client),
		lastActivity: time.Now(),
	}
	s.rooms[pid] = room
	roomsCounter.Inc()
	slog.Info("created room", "project", pid)
	return room
}

// AddClient registers a websocket client in a room
func (s *Server) AddClient(pid, uid string, ws *websocket.Conn) *Client {
	room := s.getOrCreateRoom(pid)
	client := &Client{conn: ws, userUUID: uid, projectUUID: pid}

	room.mu.Lock()
	room.clients[uid] = client
	room.lastActivity = time.Now()
	room.mu.Unlock()

	usersCounter.Inc()
	slog.Info("added client", "user", uid, "project", pid)
	return client
}

// RemoveClient unregisters and closes a websocket client
func (s *Server) RemoveClient(c *Client) {
	s.mu.RLock()
	room, ok := s.rooms[c.projectUUID]
	s.mu.RUnlock()
	if !ok {
		return
	}
	room.mu.Lock()
	delete(room.clients, c.userUUID)
	room.lastActivity = time.Now()
	room.mu.Unlock()
	c.conn.Close()
	slog.Info("removed client", "user", c.userUUID, "project", c.projectUUID)
}

// Broadcast sends a message to all other clients in the room
func (s *Server) Broadcast(pid, sender string, mt int, msg []byte) {
	s.mu.RLock()
	room, ok := s.rooms[pid]
	s.mu.RUnlock()
	if !ok {
		return
	}
	room.mu.RLock()
	targets := make([]*Client, 0, len(room.clients))
	for uid, cl := range room.clients {
		if uid != sender {
			targets = append(targets, cl)
		}
	}
	room.mu.RUnlock()
	for _, cl := range targets {
		cl.mu.Lock()
		if err := cl.conn.WriteMessage(mt, msg); err != nil {
			slog.Error("write failed", "error", err.Error(), "user", cl.userUUID, "project", pid)
		}
		cl.mu.Unlock()
	}
	room.mu.Lock()
	room.lastActivity = time.Now()
	room.mu.Unlock()
}

// UsersCount returns total websocket clients
func (s *Server) UsersCount() int {
	total := 0
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, room := range s.rooms {
		room.mu.RLock()
		total += len(room.clients)
		room.mu.RUnlock()
	}
	return total
}

// RoomsCount returns number of active rooms
func (s *Server) RoomsCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.rooms)
}

// verifySupabaseToken parses and validates a JWT via JWKS
func verifySupabaseToken(tokenString string) (string, error) {
	token, err := jwt.Parse(tokenString, jwks.Keyfunc)
	if err != nil {
		return "", fmt.Errorf("invalid token: %w", err)
	}
	if !token.Valid {
		return "", fmt.Errorf("invalid token")
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return "", fmt.Errorf("invalid claims")
	}
	sub, ok := claims["sub"].(string)
	if !ok {
		return "", fmt.Errorf("subject claim missing")
	}
	return sub, nil
}

// upgrader echoes "Bearer" so client retains the token
var upgrader = websocket.Upgrader{
	CheckOrigin:  func(r *http.Request) bool { return true },
	Subprotocols: []string{"Bearer"},
}

func main() {
	e := echo.New()
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	server := NewServer()

	// health and metrics
	e.GET("/status", func(c echo.Context) error {
		return c.JSON(http.StatusOK, echo.Map{
			"status":      "ok",
			"users_count": server.UsersCount(),
			"rooms_count": server.RoomsCount(),
		})
	})
	e.GET("/metrics", echo.WrapHandler(promhttp.Handler()))

	// websocket endpoint
	e.GET("/ws", func(c echo.Context) error {
		uid := c.QueryParam("user_uuid")
		pid := c.QueryParam("project_uuid")
		if uid == "" || pid == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "user_uuid and project_uuid required")
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
		parts := strings.SplitN(auth, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
			return echo.NewHTTPError(http.StatusBadRequest, "Authorization format must be Bearer {token}")
		}

		sub, err := verifySupabaseToken(parts[1])
		if err != nil {
			return echo.NewHTTPError(http.StatusUnauthorized, err.Error())
		}
		if sub != uid {
			return echo.NewHTTPError(http.StatusUnauthorized, "token user mismatch")
		}

		ws, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
		if err != nil {
			slog.Error("upgrade failed", "error", err.Error(), "user", uid, "project", pid)
			return err
		}

		client := server.AddClient(pid, uid, ws)
		defer server.RemoveClient(client)

		for {
			mt, msg, err := ws.ReadMessage()
			if err != nil {
				slog.Error("read failed", "error", err.Error(), "user", uid, "project", pid)
				break
			}
			server.Broadcast(pid, uid, mt, msg)
		}
		return nil
	})

	if err := e.Start(":8989"); err != nil {
		slog.Error("server start failed", "error", err.Error())
		os.Exit(1)
	}
}
