package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"
)

var (
	startTime  = time.Now()
	writeWait  = envDuration("ROOMWS_WRITE_WAIT", 10*time.Second)
	pongWait   = envDuration("ROOMWS_PONG_WAIT", 60*time.Second)
	pingPeriod = pongWait * 9 / 10
)

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("invalid %s=%q, using default %s", key, v, def)
		return def
	}
	return d
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		log.Printf("invalid %s=%q, using default %d", key, v, def)
		return def
	}
	return n
}

func envFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		log.Printf("invalid %s=%q, using default %g", key, v, def)
		return def
	}
	return f
}

func generateID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}

// clientIP extracts the real client IP, preferring X-Forwarded-For when behind a proxy.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.Index(xff, ","); i != -1 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	ip := r.RemoteAddr
	if i := strings.LastIndex(ip, ":"); i != -1 {
		return ip[:i]
	}
	return ip
}

// Message represents a WebSocket protocol message.
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
	HistoryCount int             `json:"history_count,omitempty"`
	NoEcho       bool            `json:"no_echo,omitempty"` // parsed from client; never forwarded to subscribers
	sender       *Client         // internal routing only; excluded from JSON (unexported)
	skipSelf     bool            // internal: suppress echo back to sender
}

// Client represents a connected WebSocket user.
type Client struct {
	hub  *Hub
	conn *websocket.Conn
	send chan []byte
	id   string
}

// ipLimiter tracks per-IP token-bucket rate limiters for new connections.
type ipLimiter struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	r        rate.Limit
	b        int
}

func newIPLimiter(r rate.Limit, b int) *ipLimiter {
	return &ipLimiter{
		limiters: make(map[string]*rate.Limiter),
		r:        r,
		b:        b,
	}
}

func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	lim, ok := l.limiters[ip]
	if !ok {
		lim = rate.NewLimiter(l.r, l.b)
		l.limiters[ip] = lim
	}
	return lim.Allow()
}

// Hub maintains the set of active clients and broadcasts messages.
type Hub struct {
	clients     map[*Client]bool
	rooms       map[string]map[*Client]bool
	broadcast   chan Message
	register    chan *Client
	unregister  chan *Client
	mu          sync.RWMutex
	adminRoom   string
	whitelist   map[string]bool
	upgrader    websocket.Upgrader
	historySize int
	history     map[string][]Message
	limiter     *ipLimiter // nil when rate limiting is disabled
}

func newHub() *Hub {
	adminRoom := os.Getenv("ROOMWS_ADMIN_ROOM")
	if adminRoom == "" {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			panic(fmt.Sprintf("crypto/rand failed: %v", err))
		}
		adminRoom = "admin-" + hex.EncodeToString(b)
	}

	allowed := map[string]bool{
		"localhost": true,
		"127.0.0.1": true,
	}
	if envOrigins := os.Getenv("ROOMWS_ALLOWED_ORIGINS"); envOrigins != "" {
		for _, o := range strings.Split(envOrigins, ",") {
			allowed[strings.TrimSpace(o)] = true
		}
	}

	var lim *ipLimiter
	if r := envFloat("ROOMWS_RATE_LIMIT", 5); r > 0 {
		lim = newIPLimiter(rate.Limit(r), envInt("ROOMWS_RATE_BURST", 10))
	}

	h := &Hub{
		broadcast:   make(chan Message),
		register:    make(chan *Client),
		unregister:  make(chan *Client),
		clients:     make(map[*Client]bool),
		rooms:       make(map[string]map[*Client]bool),
		adminRoom:   adminRoom,
		whitelist:   allowed,
		historySize: envInt("ROOMWS_HISTORY_SIZE", 0),
		history:     make(map[string][]Message),
		limiter:     lim,
	}
	h.upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return h.isAllowed(r.Header.Get("Origin"))
		},
	}
	return h
}

func (h *Hub) isAllowed(origin string) bool {
	if origin == "" {
		return true // allow non-browser clients and direct connections
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

// appendHistory stores a clean copy of msg in the room's history ring buffer.
// Must be called with h.mu held.
func (h *Hub) appendHistory(roomName string, msg Message) {
	if h.historySize == 0 || roomName == h.adminRoom {
		return
	}
	clean := Message{
		Type:      msg.Type,
		Room:      msg.Room,
		Message:   msg.Message,
		ID:        msg.ID,
		Timestamp: msg.Timestamp,
		ClientID:  msg.ClientID,
	}
	hist := append(h.history[roomName], clean)
	if len(hist) > h.historySize {
		hist = hist[len(hist)-h.historySize:]
	}
	h.history[roomName] = hist
}

func (h *Hub) handleAdminCommand(cmdStr string) {
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
		list := make([]string, 0, len(h.whitelist))
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

	respData, err := json.Marshal(responseText)
	if err != nil {
		log.Printf("marshal error in admin response: %v", err)
		return
	}
	h.broadcast <- Message{
		Type:      "publish",
		Room:      h.adminRoom,
		Message:   json.RawMessage(respData),
		ID:        generateID(),
		Timestamp: time.Now().UnixMilli(),
		ClientID:  "room_ws",
	}
}

func (h *Hub) run() {
	type leaveNotification struct {
		targets []*Client
		data    []byte
	}

	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; !ok {
				h.mu.Unlock()
				continue
			}
			delete(h.clients, client)
			close(client.send)

			// Collect leave notifications under the lock for a consistent snapshot,
			// then send after releasing to avoid blocking with the mutex held.
			var notifications []leaveNotification
			for roomName, subscribers := range h.rooms {
				if _, ok := subscribers[client]; !ok {
					continue
				}
				delete(subscribers, client)
				if len(subscribers) == 0 {
					delete(h.rooms, roomName)
					delete(h.history, roomName)
					continue
				}
				leaveMsg := Message{
					Type:     "member_leave",
					Room:     roomName,
					ClientID: client.id,
				}
				data, err := json.Marshal(leaveMsg)
				if err != nil {
					log.Printf("marshal error for member_leave: %v", err)
					continue
				}
				targets := make([]*Client, 0, len(subscribers))
				for other := range subscribers {
					targets = append(targets, other)
				}
				notifications = append(notifications, leaveNotification{targets, data})
			}
			h.mu.Unlock()

			for _, n := range notifications {
				for _, target := range n.targets {
					select {
					case target.send <- n.data:
					default:
					}
				}
			}

		case msg := <-h.broadcast:
			data, err := json.Marshal(msg)
			if err != nil {
				log.Printf("marshal error in broadcast: %v", err)
				continue
			}

			// Use write lock to atomically update history and snapshot subscribers.
			h.mu.Lock()
			if msg.Type == "publish" {
				h.appendHistory(msg.Room, msg)
			}
			subscribers, ok := h.rooms[msg.Room]
			if !ok {
				h.mu.Unlock()
				continue
			}
			targets := make([]*Client, 0, len(subscribers))
			for client := range subscribers {
				targets = append(targets, client)
			}
			h.mu.Unlock()

			var toRemove []*Client
			for _, client := range targets {
				if msg.skipSelf && client == msg.sender {
					continue
				}
				select {
				case client.send <- data:
				default:
					toRemove = append(toRemove, client)
				}
			}

			if len(toRemove) > 0 {
				h.mu.Lock()
				for _, client := range toRemove {
					if _, ok := h.clients[client]; ok {
						close(client.send)
						delete(h.clients, client)
					}
				}
				h.mu.Unlock()
			}
		}
	}
}

func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()

	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("error: %v", err)
			}
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
			data, err := json.Marshal(resp)
			if err != nil {
				log.Printf("marshal error in handshake: %v", err)
				continue
			}
			c.send <- data

		case "subscribe":
			roomName := msg.Room
			wantHistory := msg.HistoryCount

			c.hub.mu.Lock()
			if _, ok := c.hub.rooms[roomName]; !ok {
				c.hub.rooms[roomName] = make(map[*Client]bool)
			}
			subscribers := c.hub.rooms[roomName]

			members := make([]string, 0, len(subscribers)+1)
			joinTargets := make([]*Client, 0, len(subscribers))
			for sub := range subscribers {
				members = append(members, sub.id)
				joinTargets = append(joinTargets, sub)
			}
			members = append(members, c.id)
			subscribers[c] = true

			// Collect history snapshot while holding the lock.
			var histMsgs []Message
			if wantHistory > 0 && c.hub.historySize > 0 {
				hist := c.hub.history[roomName]
				n := wantHistory
				if n > len(hist) {
					n = len(hist)
				}
				histMsgs = make([]Message, n)
				copy(histMsgs, hist[len(hist)-n:])
			}
			c.hub.mu.Unlock()

			membersData, err := json.Marshal(members)
			if err != nil {
				log.Printf("marshal error for members list: %v", err)
				break
			}
			membersMsg := Message{
				Type:    "members",
				Room:    roomName,
				Message: json.RawMessage(membersData),
			}
			data, err := json.Marshal(membersMsg)
			if err != nil {
				log.Printf("marshal error for members message: %v", err)
				break
			}
			c.send <- data

			// Replay history oldest-first before notifying existing members.
			for _, hMsg := range histMsgs {
				d, err := json.Marshal(hMsg)
				if err != nil {
					log.Printf("marshal error for history message: %v", err)
					continue
				}
				c.send <- d
			}

			joinMsg := Message{
				Type:     "member_join",
				Room:     roomName,
				ClientID: c.id,
			}
			joinData, err := json.Marshal(joinMsg)
			if err != nil {
				log.Printf("marshal error for member_join: %v", err)
				break
			}
			for _, sub := range joinTargets {
				select {
				case sub.send <- joinData:
				default:
				}
			}

			if msg.Callback != nil {
				resp := Message{Callback: msg.Callback}
				respData, err := json.Marshal(resp)
				if err != nil {
					log.Printf("marshal error for subscribe callback: %v", err)
					break
				}
				c.send <- respData
			}

		case "unsubscribe":
			roomName := msg.Room

			c.hub.mu.Lock()
			var leaveTargets []*Client
			var leaveData []byte
			if subscribers, ok := c.hub.rooms[roomName]; ok {
				if _, ok := subscribers[c]; ok {
					delete(subscribers, c)
					if len(subscribers) == 0 {
						delete(c.hub.rooms, roomName)
						delete(c.hub.history, roomName)
					} else {
						leaveMsg := Message{
							Type:     "member_leave",
							Room:     roomName,
							ClientID: c.id,
						}
						if d, err := json.Marshal(leaveMsg); err == nil {
							leaveData = d
							for sub := range subscribers {
								leaveTargets = append(leaveTargets, sub)
							}
						} else {
							log.Printf("marshal error for member_leave: %v", err)
						}
					}
				}
			}
			c.hub.mu.Unlock()

			for _, sub := range leaveTargets {
				select {
				case sub.send <- leaveData:
				default:
				}
			}

		case "publish":
			publishMsg := Message{
				Type:      "publish",
				Room:      msg.Room,
				Message:   msg.Message,
				ID:        generateID(),
				Timestamp: time.Now().UnixMilli(),
				ClientID:  c.id,
				sender:    c,
				skipSelf:  msg.NoEcho,
			}
			c.hub.broadcast <- publishMsg
			if msg.Room == c.hub.adminRoom {
				go c.hub.handleAdminCommand(string(msg.Message))
			}
		}
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()

	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				log.Printf("write error: %v", err)
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func serveWs(hub *Hub, w http.ResponseWriter, r *http.Request) {
	if hub.limiter != nil && !hub.limiter.allow(clientIP(r)) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}
	conn, err := hub.upgrader.Upgrade(w, r, nil)
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

func healthCheck(port string) {
	resp, err := http.Get("http://localhost:" + port + "/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
	os.Exit(0)
}

func main() {
	port := os.Getenv("ROOMWS_PORT")
	if port == "" {
		port = "8080"
	}

	if len(os.Args) > 1 && os.Args[1] == "-health" {
		healthCheck(port)
		return
	}

	hub := newHub()
	go hub.run()

	log.Printf("Admin room name: %s", hub.adminRoom)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		hub.mu.RLock()
		clients := len(hub.clients)
		rooms := len(hub.rooms)
		hub.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"uptime":  time.Since(startTime).Round(time.Second).String(),
			"clients": clients,
			"rooms":   rooms,
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
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

	server := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		log.Println("Shutting down server...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("Shutdown error: %v", err)
		}
	}()

	log.Printf("Server starting on :%s", port)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal("ListenAndServe: ", err)
	}
	log.Println("Server stopped.")
}
