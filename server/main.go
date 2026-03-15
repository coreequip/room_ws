package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var (
	startTime = time.Now()
)

func generateID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Message represents the ScaleDrone protocol format
type Message struct {
	Type         string          `json:"type,omitempty"`
	Channel      string          `json:"channel,omitempty"`
	Version      int             `json:"version,omitempty"`
	Room         string          `json:"room,omitempty"`
	Message      json.RawMessage `json:"message,omitempty"`
	Callback     *int            `json:"callback,omitempty"`
	ClientID     string          `json:"client_id,omitempty"`
	RequireAuth  bool            `json:"require_auth,omitempty"`
	Error        string          `json:"error,omitempty"`
	ID           string          `json:"id,omitempty"`
	Timestamp    int64           `json:"timestamp,omitempty"`
}

// Client represents a connected user
type Client struct {
	hub  *Hub
	conn *websocket.Conn
	send chan []byte
	id   string
}

// Hub maintains the set of active clients and broadcasts messages
type Hub struct {
	clients    map[*Client]bool
	rooms      map[string]map[*Client]bool
	broadcast  chan Message
	register   chan *Client
	unregister chan *Client
	mu         sync.RWMutex
	adminRoom  string
	whitelist  map[string]bool
}

func newHub() *Hub {
	adminRoom := os.Getenv("ROOMWS_ADMIN_ROOM")
	if adminRoom == "" {
		b := make([]byte, 8)
		rand.Read(b)
		adminRoom = "admin-" + hex.EncodeToString(b)
	}

	allowed := make(map[string]bool)
	allowed["localhost"] = true
	allowed["127.0.0.1"] = true

	envOrigins := os.Getenv("ROOMWS_ALLOWED_ORIGINS")
	if envOrigins != "" {
		for _, o := range strings.Split(envOrigins, ",") {
			allowed[strings.TrimSpace(o)] = true
		}
	}

	return &Hub{
		broadcast:  make(chan Message),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		clients:    make(map[*Client]bool),
		rooms:      make(map[string]map[*Client]bool),
		adminRoom:  adminRoom,
		whitelist:  allowed,
	}
}

func (h *Hub) isAllowed(origin string) bool {
	if origin == "" {
		return true // Allow non-browser clients or direct connections
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Hostname()

	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.whitelist[host] {
		return true
	}

	for allowed := range h.whitelist {
		if strings.HasSuffix(host, "."+allowed) {
			return true
		}
	}
	return false
}

func (h *Hub) handleAdminCommand(cmdStr string) {
	// Clean up the command string (it might be a JSON quoted string)
	var cmdText string
	if err := json.Unmarshal([]byte(cmdStr), &cmdText); err != nil {
		cmdText = cmdStr
	}
	cmdText = strings.TrimSpace(cmdText)
	parts := strings.Fields(cmdText)
	if len(parts) == 0 {
		return
	}

	var responseText string
	command := strings.ToLower(parts[0])

	switch command {
	case "add":
		if len(parts) > 1 {
			domain := parts[1]
			h.mu.Lock()
			h.whitelist[domain] = true
			h.mu.Unlock()
			responseText = fmt.Sprintf("Domain %s added to whitelist.", domain)
		} else {
			responseText = "Usage: add <domain>"
		}
	case "remove":
		if len(parts) > 1 {
			domain := parts[1]
			if domain == "localhost" || domain == "127.0.0.1" {
				responseText = "Cannot remove protected domain."
			} else {
				h.mu.Lock()
				delete(h.whitelist, domain)
				h.mu.Unlock()
				responseText = fmt.Sprintf("Domain %s removed from whitelist.", domain)
			}
		} else {
			responseText = "Usage: remove <domain>"
		}
	case "list":
		h.mu.RLock()
		var list []string
		for d := range h.whitelist {
			list = append(list, d)
		}
		h.mu.RUnlock()
		responseText = "Whitelisted domains: " + strings.Join(list, ", ")
	case "status":
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		h.mu.RLock()
		numClients := len(h.clients)
		numRooms := len(h.rooms)
		h.mu.RUnlock()

		responseText = fmt.Sprintf(
			"Status:\n- Uptime: %s\n- Clients: %d\n- Rooms: %d\n- Goroutines: %d\n- Memory: %.2f MB",
			time.Since(startTime).Round(time.Second),
			numClients,
			numRooms,
			runtime.NumGoroutine(),
			float64(m.Alloc)/1024/1024,
		)
	default:
		responseText = "Unknown command. Available: add, remove, list, status"
	}

	// Send response as room_ws
	respData, _ := json.Marshal(responseText)
	msg := Message{
		Type:      "publish",
		Room:      h.adminRoom,
		Message:   json.RawMessage(respData),
		ID:        generateID(),
		Timestamp: time.Now().UnixMilli(),
		ClientID:  "room_ws",
	}
	h.broadcast <- msg
}

func (h *Hub) run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
				// Remove from all rooms and notify
				for roomName, subscribers := range h.rooms {
					if _, ok := subscribers[client]; ok {
						delete(subscribers, client)
						// Always notify others of a leave
						leaveMsg := Message{
							Type:     "member_leave",
							Room:     roomName,
							ClientID: client.id,
						}
						data, _ := json.Marshal(leaveMsg)
						for other := range subscribers {
							other.send <- data
						}
					}
				}
			}
			h.mu.Unlock()
		case msg := <-h.broadcast:
			h.mu.RLock()
			if subscribers, ok := h.rooms[msg.Room]; ok {
				data, _ := json.Marshal(msg)
				for client := range subscribers {
					select {
					case client.send <- data:
					default:
						close(client.send)
						delete(h.clients, client)
					}
				}
			}
			h.mu.RUnlock()
		}
	}
}

func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			log.Printf("error: %v", err)
			break
		}

		var msg Message
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Printf("unmarshal error: %v", err)
			continue
		}

		switch msg.Type {
		case "handshake":
			resp := Message{
				Callback:    msg.Callback,
				ClientID:    c.id,
				RequireAuth: false,
			}
			data, _ := json.Marshal(resp)
			c.send <- data

		case "subscribe":
			c.hub.mu.Lock()
			roomName := msg.Room
			if _, ok := c.hub.rooms[roomName]; !ok {
				c.hub.rooms[roomName] = make(map[*Client]bool)
			}
			subscribers := c.hub.rooms[roomName]

			// Send current member list
			members := make([]string, 0, len(subscribers)+1)
			for sub := range subscribers {
				members = append(members, sub.id)
			}
			members = append(members, c.id)

			membersData, _ := json.Marshal(members)
			membersMsg := Message{
				Type:    "members",
				Room:    roomName,
				Message: json.RawMessage(membersData),
			}
			data, _ := json.Marshal(membersMsg)
			c.send <- data

			// Notify existing members
			joinMsg := Message{
				Type:     "member_join",
				Room:     roomName,
				ClientID: c.id,
			}
			joinData, _ := json.Marshal(joinMsg)
			for sub := range subscribers {
				sub.send <- joinData
			}

			subscribers[c] = true
			c.hub.mu.Unlock()

			if msg.Callback != nil {
				resp := Message{Callback: msg.Callback}
				data, _ := json.Marshal(resp)
				c.send <- data
			}

		case "unsubscribe":
			c.hub.mu.Lock()
			roomName := msg.Room
			if subscribers, ok := c.hub.rooms[roomName]; ok {
				if _, ok := subscribers[c]; ok {
					delete(subscribers, c)
					// Always notify
					leaveMsg := Message{
						Type:     "member_leave",
						Room:     roomName,
						ClientID: c.id,
					}
					data, _ := json.Marshal(leaveMsg)
					for sub := range subscribers {
						sub.send <- data
					}
				}
			}
			c.hub.mu.Unlock()

		case "publish":
			publishMsg := Message{
				Type:      "publish",
				Room:      msg.Room,
				Message:   msg.Message,
				ID:        generateID(),
				Timestamp: time.Now().UnixMilli(),
				ClientID:  c.id,
			}
			c.hub.broadcast <- publishMsg

			// If it's the admin room, handle command
			if msg.Room == c.hub.adminRoom {
				go c.hub.handleAdminCommand(string(msg.Message))
			}
		}
	}
}

func (c *Client) writePump() {
	for {
		select {
		case message, ok := <-c.send:
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			c.conn.WriteMessage(websocket.TextMessage, message)
		}
	}
}

func serveWs(hub *Hub, w http.ResponseWriter, r *http.Request) {
	var upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return hub.isAllowed(r.Header.Get("Origin"))
		},
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	client := &Client{
		hub:  hub,
		conn: conn,
		send: make(chan []byte, 256),
		id:   generateID(),
	}
	client.hub.register <- client

	go client.writePump()
	go client.readPump()
}

func main() {
	hub := newHub()
	go hub.run()

	log.Printf("Admin room name: %s", hub.adminRoom)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		serveWs(hub, w, r)
	})

	addr := ":8080"
	log.Printf("Server starting on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
