package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	idleRoomTTL   = 15 * time.Minute
	verifyTimeout = 15 * time.Second
	writeTimeout  = 5 * time.Second
)

type Client struct {
	conn      *websocket.Conn
	userID    string
	projectID string
	mu        sync.Mutex
}

// thread-safe write with deadline
func (c *Client) write(mt int, msg []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return c.conn.WriteMessage(mt, msg)
}

type ProjectRoom struct {
	clients      map[*Client]struct{}
	mu           sync.RWMutex
	lastActivity time.Time
}

type Server struct {
	rooms map[string]*ProjectRoom
	mu    sync.RWMutex
}

var (
	usersCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "connected_users_total",
		Help: "Total websocket connections ever established",
	})
	roomsCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rooms_total",
		Help: "Total project rooms ever created",
	})

	supabaseURL     = strings.TrimRight(os.Getenv("SUPABASE_URL"), "/")
	supabaseAnonKey = os.Getenv("SUPABASE_ANON_KEY")
)

func init() {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{AddSource: false})
	slog.SetDefault(slog.New(handler))

	if supabaseURL == "" {
		slog.Error("SUPABASE_URL is not set")
	}
	if supabaseAnonKey == "" {
		slog.Error("SUPABASE_ANON_KEY is not set; admin-verify will fail")
	} else {
		slog.Info("Loaded Supabase service key")
	}
	prometheus.MustRegister(usersCounter, roomsCounter)
}

// verifyViaAdmin validates the user's JWT with Supabase.
func verifyViaAdmin(userJWT string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), verifyTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/auth/v1/user", supabaseURL), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+userJWT)
	req.Header.Set("apikey", supabaseAnonKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("admin-verify HTTP error", "err", err.Error())
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Warn("admin-verify failed", "status", resp.StatusCode) // body dropped
		return "", fmt.Errorf("admin-verify failed: %d", resp.StatusCode)
	}

	var user struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		slog.Error("admin-verify decode error", "err", err.Error())
		return "", err
	}
	if user.ID == "" {
		slog.Warn("admin-verify missing id claim")
		return "", fmt.Errorf("admin-verify missing user.id")
	}
	slog.Info("admin-verify success", "id", user.ID)
	return user.ID, nil
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
		now := time.Now()
		s.mu.Lock()
		var stale []*ProjectRoom
		for pid, room := range s.rooms {
			room.mu.RLock()
			idle := now.Sub(room.lastActivity) > idleRoomTTL
			room.mu.RUnlock()
			if idle {
				stale = append(stale, room)
				delete(s.rooms, pid)
				slog.Info("cleaned up room", "project", pid)
			}
		}
		s.mu.Unlock()

		// close conns outside global lock
		for _, room := range stale {
			room.mu.RLock()
			for c := range room.clients {
				_ = c.write(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure,
						"room idle, reconnect"))
				_ = c.conn.Close()
			}
			room.mu.RUnlock()
		}
	}
}

func (s *Server) getOrCreateRoom(projectID string) *ProjectRoom {
	s.mu.Lock()
	defer s.mu.Unlock()
	room, ok := s.rooms[projectID]
	if !ok {
		room = &ProjectRoom{
			clients:      make(map[*Client]struct{}),
			lastActivity: time.Now(),
		}
		s.rooms[projectID] = room
		roomsCounter.Inc()
		slog.Info("created new room", "project", projectID)
	}
	return room
}

func (s *Server) AddClient(projectID, userID string, ws *websocket.Conn) *Client {
	room := s.getOrCreateRoom(projectID)
	client := &Client{conn: ws, userID: userID, projectID: projectID}

	room.mu.Lock()
	room.clients[client] = struct{}{}
	room.lastActivity = time.Now()
	room.mu.Unlock()

	usersCounter.Inc()
	slog.Info("added client", "user", userID, "project", projectID)
	return client
}

func (s *Server) RemoveClient(c *Client) {
	s.mu.RLock()
	room, ok := s.rooms[c.projectID]
	s.mu.RUnlock()
	if !ok {
		return
	}
	room.mu.Lock()
	delete(room.clients, c)
	room.lastActivity = time.Now()
	room.mu.Unlock()

	_ = c.conn.Close()
	c.conn = nil // help GC
	slog.Info("removed client", "user", c.userID, "project", c.projectID)
}

// Broadcast avoids holding locks during network I/O
func (s *Server) Broadcast(projectID string, sender *Client, mt int, msg []byte) {
	s.mu.RLock()
	room, ok := s.rooms[projectID]
	s.mu.RUnlock()
	if !ok {
		return
	}

	// snapshot clients
	room.mu.RLock()
	targets := make([]*Client, 0, len(room.clients))
	for c := range room.clients {
		if c != sender {
			targets = append(targets, c)
		}
	}
	room.mu.RUnlock()

	for _, c := range targets {
		if err := c.write(mt, msg); err != nil {
			slog.Error("broadcast write failed", "err", err.Error(), "to", c.userID)
		}
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

var upgrader = websocket.Upgrader{
	CheckOrigin:  func(r *http.Request) bool { return true }, // CORS ignored per requirement
	Subprotocols: []string{"Bearer"},
}

func main() {
	e := echo.New()
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Pre(middleware.Rewrite(map[string]string{
		"/collab/*": "/$1",
		"/collab":   "/",
	}))

	server := NewServer()

	// Health check endpoint
	e.GET("/", func(c echo.Context) error {
		return c.JSON(http.StatusOK, echo.Map{
			"status":  "ok",
			"version": "0.0.4", // bumped version after idle-close change
		})
	})

	// Easy to ready metrics endpoint
	e.GET("/status", func(c echo.Context) error {
		return c.JSON(http.StatusOK, echo.Map{
			"status":      "ok",
			"users_count": server.UsersCount(),
			"rooms_count": server.RoomsCount(),
		})
	})

	// Scrapping endpoint for Prometheus
	e.GET("/metrics", echo.WrapHandler(promhttp.Handler()))

	e.GET("/ws", func(c echo.Context) error {
		userID := c.QueryParam("user_id")
		projectID := c.QueryParam("project_id")
		if userID == "" || projectID == "" {
			slog.Error("missing query params", "user_id", userID, "project_id", projectID)
			return echo.NewHTTPError(http.StatusBadRequest, "user_id and project_id required")
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
			slog.Error("authorization missing")
			return echo.NewHTTPError(http.StatusBadRequest, "Authorization required")
		}
		parts := strings.SplitN(auth, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
			slog.Error("invalid authorization format", "header", auth)
			return echo.NewHTTPError(http.StatusBadRequest, "Authorization must be Bearer {token}")
		}

		userJWT := parts[1]
		sub, err := verifyViaAdmin(userJWT)
		if err != nil {
			slog.Warn("token validation failed", "err", err.Error())
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid token")
		}
		if sub != userID {
			slog.Error("token user mismatch", "token_sub", sub, "query_user_id", userID)
			return echo.NewHTTPError(http.StatusUnauthorized, "token user mismatch")
		}

		ws, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
		if err != nil {
			slog.Error("websocket upgrade failed", "err", err.Error())
			return err
		}

		client := server.AddClient(projectID, userID, ws)
		defer server.RemoveClient(client)

		for {
			mt, msg, err := ws.ReadMessage()
			if err != nil {
				if !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
					slog.Error("read message failed", "err", err.Error())
				}
				break
			}
			server.Broadcast(projectID, client, mt, msg)
		}
		return nil
	})

	if err := e.Start(":8989"); err != nil {
		slog.Error("server start failed", "err", err.Error())
		os.Exit(1)
	}
}
