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
	usersCounter = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "connected_users_total",
			Help: "Total websocket users ever connected",
		},
	)
	roomsCounter = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "rooms_total",
			Help: "Total project rooms ever created",
		},
	)

	// Supabase configuration from environment
	supabaseURL        = os.Getenv("SUPABASE_URL")
	supabaseAnonKey    = os.Getenv("SUPABASE_ANON_KEY")
	supabaseServiceKey = os.Getenv("SUPABASE_SERVICE_KEY")

	// jwks holds the JSON Web Key Set for verifying Supabase JWTs
	jwks *keyfunc.JWKS
)

func init() {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{AddSource: false})

	if supabaseURL == "" {
		slog.Error("SUPABASE_URL is not set")
	}
	if supabaseAnonKey == "" {
		slog.Error("SUPABASE_ANON_KEY is not set")
	}
	if supabaseServiceKey == "" {
		slog.Error("SUPABASE_SERVICE_KEY is not set")
	}
	slog.Info("Supabase env vars loaded", "url", supabaseURL)

	slog.SetDefault(slog.New(handler))
	prometheus.MustRegister(usersCounter, roomsCounter)

	// Initialize JWKS from Supabase
	jwksURL := fmt.Sprintf("%s/auth/v1/.well-known/jwks.json", supabaseURL)
	var err error
	jwks, err = keyfunc.Get(jwksURL, keyfunc.Options{
		RefreshInterval:    time.Hour, // refresh JWKs every hour
		RefreshErrorLogger: slog.New(func(r slog.Record) error { r.Log(); return nil }, nil),
	})
	if err != nil {
		slog.Error("failed to get JWKS", "url", jwksURL, "error", err.Error())
		os.Exit(1)
	}
}

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
	s := &Server{rooms: make(map[string]*ProjectRoom)}
	go s.runCleanup()
	return s
}

func (s *Server) runCleanup() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-15 * time.Minute)
		s.mu.Lock()
		for pid, room := range s.rooms {
			room.mu.RLock()
			last := room.lastActivity
			room.mu.RUnlock()
			if last.Before(cutoff) {
				delete(s.rooms, pid)
				slog.Info("cleaned up room", "project", pid)
			}
		}
		s.mu.Unlock()
	}
}

func (s *Server) getOrCreateRoom(pid string) *ProjectRoom {
	s.mu.Lock()
	defer s.mu.Unlock()
	room, ok := s.rooms[pid]
	if !ok {
		room = &ProjectRoom{clients: make(map[string]*Client), lastActivity: time.Now()}
		s.rooms[pid] = room
		roomsCounter.Inc()
		slog.Info("created new room", "project", pid)
	}
	return room
}

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

func (s *Server) Broadcast(pid, sender string, mt int, msg []byte) {
	s.mu.RLock()
	room, ok := s.rooms[pid]
	s.mu.RUnlock()
	if !ok {
		return
	}
	room.mu.RLock()
	targets := make([]*Client, 0, len(room.clients))
	for uid, c := range room.clients {
		if uid != sender {
			targets = append(targets, c)
		}
	}
	room.mu.RUnlock()
	for _, c := range targets {
		c.mu.Lock()
		if err := c.conn.WriteMessage(mt, msg); err != nil {
			slog.Error("write message failed", "error", err.Error(), "user", c.userUUID, "project", pid)
		}
		c.mu.Unlock()
	}
	room.mu.Lock()
	room.lastActivity = time.Now()
	room.mu.Unlock()
}

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

func (s *Server) RoomsCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.rooms)
}

func verifySupabaseToken(tokenString string) (string, error) {
	// Parse and verify the token using the JWKS
	token, err := jwt.Parse(tokenString, jwks.Keyfunc)
	if err != nil {
		slog.Error("token parse/verify error", "error", err.Error())
		return "", fmt.Errorf("invalid token: %w", err)
	}
	if !token.Valid {
		slog.Error("invalid token", "token", tokenString)
		return "", fmt.Errorf("invalid token")
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		slog.Error("invalid token claims")
		return "", fmt.Errorf("invalid token claims")
	}
	sub, ok := claims["sub"].(string)
	if !ok {
		slog.Error("subject claim missing or invalid")
		return "", fmt.Errorf("subject claim missing or invalid")
	}
	return sub, nil
}

var upgrader = websocket.Upgrader{
	CheckOrigin:  func(r *http.Request) bool { return true },
	Subprotocols: []string{"Bearer"}, // echo back to client
}

func main() {
	e := echo.New()
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	server := NewServer()

	e.GET("/status", func(c echo.Context) error {
		return c.JSON(http.StatusOK, echo.Map{
			"status":      "ok",
			"users_count": server.UsersCount(),
			"rooms_count": server.RoomsCount(),
		})
	})

	e.GET("/metrics", echo.WrapHandler(promhttp.Handler()))

	e.GET("/ws", func(c echo.Context) error {
		uid := c.QueryParam("user_uuid")
		pid := c.QueryParam("project_uuid")
		if uid == "" || pid == "" {
			slog.Error("missing query params", "user_uuid", uid, "project_uuid", pid)
			return echo.NewHTTPError(http.StatusBadRequest, "user_uuid and project_uuid required")
		}

		authHeader := c.Request().Header.Get("Authorization")
		if authHeader == "" {
			if protoHdr := c.Request().Header.Get("Sec-WebSocket-Protocol"); protoHdr != "" {
				parts := strings.Split(protoHdr, ",")
				if len(parts) == 2 && strings.TrimSpace(parts[0]) == "Bearer" {
					authHeader = "Bearer " + strings.TrimSpace(parts[1])
				}
			}
		}
		if authHeader == "" {
			slog.Error("authorization missing")
			return echo.NewHTTPError(http.StatusBadRequest, "Authorization required (header or sub-protocol)")
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			slog.Error("invalid authorization format", "header", authHeader)
			return echo.NewHTTPError(http.StatusBadRequest, "Authorization format must be Bearer {token}")
		}

		tokenString := parts[1]
		validatedUUID, err := verifySupabaseToken(tokenString)
		if err != nil {
			slog.Error("token verification failed", "error", err.Error())
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid token")
		}
		if validatedUUID != uid {
			slog.Error("token user mismatch", "token_user", validatedUUID, "request_user", uid)
			return echo.NewHTTPError(http.StatusUnauthorized, "token user mismatch")
		}

		ws, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
		if err != nil {
			slog.Error("websocket upgrade failed", "error", err.Error(), "user", uid, "project", pid)
			return err
		}

		client := server.AddClient(pid, uid, ws)
		defer server.RemoveClient(client)

		for {
			mt, msg, err := ws.ReadMessage()
			if err != nil {
				slog.Error("read message failed", "error", err.Error(), "user", uid, "project", pid)
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
