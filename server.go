// server.go
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// Prometheus metrics
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

	// Supabase JWT secret from environment
	supabaseJWTSecret = os.Getenv("SUPABASE_JWT_SECRET")
)

func init() {
	prometheus.MustRegister(usersCounter, roomsCounter)
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
		_ = c.conn.WriteMessage(mt, msg)
		c.mu.Unlock()
	}
	room.mu.Lock()
	room.lastActivity = time.Now()
	room.mu.Unlock()
}

func (s *Server) UsersCount() (total int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, room := range s.rooms {
		room.mu.RLock()
		total += len(room.clients)
		room.mu.RUnlock()
	}
	return
}

func (s *Server) RoomsCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.rooms)
}

var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

// verifySupabaseToken parses and validates a Supabase JWT, returning the user UUID
func verifySupabaseToken(tokenString string) (string, error) {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		// Ensure token method is HMAC
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(supabaseJWTSecret), nil
	})
	if err != nil || !token.Valid {
		return "", fmt.Errorf("invalid token: %w", err)
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return "", fmt.Errorf("invalid token claims")
	}
	sub, ok := claims["sub"].(string)
	if !ok {
		return "", fmt.Errorf("subject claim missing or invalid")
	}
	return sub, nil
}

/* -------------------------------------------------------------------------
 * main
 * -----------------------------------------------------------------------*/
var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

func main() {
	e := echo.New()
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	server := NewServer()

	// health/status endpoint
	e.GET("/status", func(c echo.Context) error {
		return c.JSON(http.StatusOK, echo.Map{
			"status":      "ok",
			"users_count": server.UsersCount(),
			"rooms_count": server.RoomsCount(),
		})
	})

	e.GET("/metrics", echo.WrapHandler(promhttp.Handler()))

	// WebSocket endpoint with Supabase JWT check
	e.GET("/ws", func(c echo.Context) error {
		uid := c.QueryParam("user_uuid")
		pid := c.QueryParam("project_uuid")
		if uid == "" || pid == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "user_uuid and project_uuid required")
		}

		/* 1️⃣ Try normal Authorization header (CLI / Node clients) */
		authHeader := c.Request().Header.Get("Authorization")

		/* 2️⃣ Fallback: extract JWT from Sec-WebSocket-Protocol
		   Browser sends:  "Bearer, <token>"
		*/
		if authHeader == "" {
			if protoHdr := c.Request().Header.Get("Sec-WebSocket-Protocol"); protoHdr != "" {
				parts := strings.Split(protoHdr, ",")
				if len(parts) == 2 && strings.TrimSpace(parts[0]) == "Bearer" {
					authHeader = "Bearer " + strings.TrimSpace(parts[1])
				}
			}
		}
		if authHeader == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "Authorization required (header or sub-protocol)")
		}

		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			return echo.NewHTTPError(http.StatusBadRequest, "Authorization format must be Bearer {token}")
		}
		tokenString := parts[1]

		// Verify JWT and match UUID
		validatedUUID, err := verifySupabaseToken(tokenString)
		if err != nil {
			return echo.NewHTTPError(http.StatusUnauthorized, "invalid token")
		}
		if validatedUUID != uid {
			return echo.NewHTTPError(http.StatusUnauthorized, "token user mismatch")
		}

		ws, err := upgrader.Upgrade(c.Response(), c.Request(), nil)
		if err != nil {
			return err
		}

		client := server.AddClient(pid, uid, ws)
		defer server.RemoveClient(client)
		for {
			mt, msg, err := ws.ReadMessage()
			if err != nil {
				break
			}
			server.Broadcast(pid, uid, mt, msg)
		}
		return nil
	})

	e.Static("/", "www/dist")

	log.Fatal(e.Start(":8989"))
}
