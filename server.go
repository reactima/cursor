// server.go
package main

import (
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

/* ───── metrics ───── */

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
)

func init() {
	prometheus.MustRegister(usersCounter, roomsCounter)
}

/* ───── data ───── */

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

/* ───── server helpers ───── */

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

func (s *Server) UsersCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	total := 0
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

/* ───── main ───── */

var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

func main() {
	e := echo.New()
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	server := NewServer()

	/* status endpoint */
	e.GET("/status", func(c echo.Context) error {
		return c.JSON(http.StatusOK, echo.Map{
			"status":      "ok",
			"users_count": server.UsersCount(),
			"rooms_count": server.RoomsCount(),
		})
	})

	/* prometheus metrics */
	e.GET("/metrics", echo.WrapHandler(promhttp.Handler()))

	/* websocket */
	e.GET("/ws", func(c echo.Context) error {
		uid := c.QueryParam("user_uuid")
		pid := c.QueryParam("project_uuid")
		if uid == "" || pid == "" {
			return echo.NewHTTPError(http.StatusBadRequest, "user_uuid and project_uuid required")
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

	/* serve react build */
	e.Static("/", "www/dist")

	log.Fatal(e.Start(":8989"))
}
