package main

import (
	// standard libs
	"log"
	"net/http"
	"sync"
	"time"

	// third-party
	"github.com/gorilla/websocket"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

// Client represents a single WebSocket connection
type Client struct {
	conn        *websocket.Conn
	userUUID    string
	projectUUID string
}

// ProjectRoom holds all clients in a project and tracks last activity
type ProjectRoom struct {
	clients      map[string]*Client
	mu           sync.RWMutex
	lastActivity time.Time
}

// Server holds all project rooms in memory
type Server struct {
	rooms map[string]*ProjectRoom
	mu    sync.RWMutex
}

// NewServer initializes the server and starts cleanup goroutine
func NewServer() *Server {
	s := &Server{
		rooms: make(map[string]*ProjectRoom),
	}
	go s.runCleanup()
	return s
}

// runCleanup removes inactive project rooms every minute
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

// getOrCreateRoom fetches or creates a ProjectRoom
func (s *Server) getOrCreateRoom(pid string) *ProjectRoom {
	s.mu.Lock()
	defer s.mu.Unlock()
	room, exists := s.rooms[pid]
	if !exists {
		room = &ProjectRoom{
			clients:      make(map[string]*Client),
			lastActivity: time.Now(),
		}
		s.rooms[pid] = room
	}
	return room
}

// AddClient registers a new client in a project room
func (s *Server) AddClient(pid, uid string, conn *websocket.Conn) *Client {
	room := s.getOrCreateRoom(pid)
	client := &Client{conn: conn, userUUID: uid, projectUUID: pid}
	room.mu.Lock()
	room.clients[uid] = client
	room.lastActivity = time.Now()
	room.mu.Unlock()
	return client
}

// RemoveClient unregisters a client and updates activity
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

// Broadcast sends a message to all other clients in the same project
func (s *Server) Broadcast(pid, senderUID string, msgType int, msg []byte) {
	s.mu.RLock()
	room, ok := s.rooms[pid]
	s.mu.RUnlock()
	if !ok {
		return
	}
	room.mu.RLock()
	defer room.mu.RUnlock()
	for uid, client := range room.clients {
		if uid == senderUID {
			continue
		}
		if err := client.conn.WriteMessage(msgType, msg); err != nil {
			// ignore write errors
			continue
		}
	}
	room.mu.Lock()
	room.lastActivity = time.Now()
	room.mu.Unlock()
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // adjust in prod
	},
}

func main() {
	e := echo.New()
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	server := NewServer()

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
			msgType, msg, err := ws.ReadMessage()
			if err != nil {
				break
			}
			// broadcast raw message to other users in same project
			server.Broadcast(pid, uid, msgType, msg)
		}
		return nil
	})

	log.Fatal(e.Start(":8989"))
}
