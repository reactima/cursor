package main

import (
	"encoding/json"
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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	usersCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "connected_users_total", Help: "Total websocket users ever connected",
	})
	roomsCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rooms_total", Help: "Total project rooms ever created",
	})

	supabaseURL        = strings.TrimRight(os.Getenv("SUPABASE_URL"), "/")
	supabaseServiceKey = os.Getenv("SUPABASE_SERVICE_KEY")
)

func init() {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{AddSource: false})
	slog.SetDefault(slog.New(handler))

	if supabaseURL == "" {
		slog.Error("SUPABASE_URL is not set")
	}
	if supabaseServiceKey == "" {
		slog.Error("SUPABASE_SERVICE_KEY is not set; admin-verify will fail")
	} else {
		slog.Info("Loaded Supabase service key")
	}
	prometheus.MustRegister(usersCounter, roomsCounter)
}

// verifyViaAdmin asks Supabase to validate the user's JWT.
// GET /auth/v1/user returning the user object directly.
func verifyViaAdmin(userJWT string) (string, error) {
	endpoint := fmt.Sprintf("%s/auth/v1/user", supabaseURL)
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+userJWT)
	req.Header.Set("apikey", supabaseServiceKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("admin-verify HTTP error", "err", err.Error())
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		slog.Warn("admin-verify failed", "status", resp.StatusCode, "body", string(body))
		return "", fmt.Errorf("admin-verify failed: %d", resp.StatusCode)
	}

	var user struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &user); err != nil {
		slog.Error("admin-verify decode error", "err", err.Error())
		return "", err
	}
	if user.ID == "" {
		slog.Warn("admin-verify missing id claim", "body", string(body))
		return "", fmt.Errorf("admin-verify missing user.id")
	}
	slog.Warn("admin-verify success", "id", user.ID)
	return user.ID, nil
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
			if room.lastActivity.Before(cutoff) {
				delete(s.rooms, pid)
				slog.Info("cleaned up room", "project", pid)
			}
			room.mu.RUnlock()
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
	for uid, client := range room.clients {
		if uid == sender {
			continue
		}
		client.mu.Lock()
		if err := client.conn.WriteMessage(mt, msg); err != nil {
			slog.Error("broadcast write failed", "err", err.Error(), "to", uid)
		}
		client.mu.Unlock()
	}
	room.mu.RUnlock()
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
	CheckOrigin:  func(r *http.Request) bool { return true },
	Subprotocols: []string{"Bearer"},
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
		if sub != uid {
			slog.Error("token user mismatch", "token_sub", sub, "query_uid", uid)
			return echo.NewHTTPError(http.StatusUnauthorized, "token user mismatch")
		}

		ws, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
		if err != nil {
			slog.Error("websocket upgrade failed", "err", err.Error())
			return err
		}
		client := server.AddClient(pid, uid, ws)
		defer server.RemoveClient(client)

		for {
			mt, msg, err := ws.ReadMessage()
			if err != nil {
				slog.Error("read message failed", "err", err.Error())
				break
			}
			server.Broadcast(pid, uid, mt, msg)
		}
		return nil
	})

	if err := e.Start(":8989"); err != nil {
		slog.Error("server start failed", "err", err.Error())
		os.Exit(1)
	}
}
