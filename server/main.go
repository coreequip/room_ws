package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
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

	// maxMessageSize caps incoming WebSocket frames to prevent memory exhaustion
	// from oversized payloads.
	maxMessageSize = int64(envInt("ROOMWS_MAX_MESSAGE_SIZE", 32*1024))

	// Per-client publish rate limiting (token bucket). Set ROOMWS_CLIENT_PUBLISH_RATE
	// to 0 to disable.
	clientPublishRate  = envFloat("ROOMWS_CLIENT_PUBLISH_RATE", 20)
	clientPublishBurst = envInt("ROOMWS_CLIENT_PUBLISH_BURST", 40)
)

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		slog.Warn("invalid duration env var, using default", "key", key, "value", v, "default", def)
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
		slog.Warn("invalid int env var, using default", "key", key, "value", v, "default", def)
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
	if err != nil || f < 0 {
		slog.Warn("invalid float env var, using default", "key", key, "value", v, "default", def)
		return def
	}
	return f
}

func generateID() string {
	return randomToken(8)
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
	hub        *Hub
	conn       *websocket.Conn
	send       chan []byte
	id         string
	pubLimiter *rate.Limiter // nil when publish rate limiting is disabled
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
	clients      map[*Client]bool
	rooms        map[string]map[*Client]bool
	broadcast    chan Message
	register     chan *Client
	unregister   chan *Client
	mu           sync.RWMutex
	adminRoom    string
	whitelist    map[string]bool
	upgrader     websocket.Upgrader
	historySize  int
	history      map[string][]Message
	limiter      *ipLimiter // nil when rate limiting is disabled
	metricsToken string
	turn         *turnConfig // nil when no TURN relay is configured
}

// randomToken returns a random hex-encoded token of n bytes.
func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}

// turnConfig holds the shared secret and URLs for minting ephemeral TURN
// credentials. It is nil unless ROOMWS_TURN_SECRET is set, which keeps the
// server a plain pub/sub server when no TURN relay is attached.
type turnConfig struct {
	secret string
	urls   []string
	ttl    time.Duration
}

func newTurnConfig() *turnConfig {
	secret := os.Getenv("ROOMWS_TURN_SECRET")
	if secret == "" {
		return nil
	}
	var urls []string
	for _, u := range strings.Split(os.Getenv("ROOMWS_TURN_URLS"), ",") {
		if u = strings.TrimSpace(u); u != "" {
			urls = append(urls, u)
		}
	}
	return &turnConfig{
		secret: secret,
		urls:   urls,
		ttl:    envDuration("ROOMWS_TURN_TTL", 2*time.Hour),
	}
}

// credentials mints a coturn REST-API credential pair: the username carries the
// expiry, the password is its HMAC. SHA-1 is not a choice here — coturn's
// use-auth-secret mode prescribes HMAC-SHA1.
func (c *turnConfig) credentials(now time.Time) (username, credential string) {
	username = fmt.Sprintf("%d:%s", now.Add(c.ttl).Unix(), randomToken(4))
	mac := hmac.New(sha1.New, []byte(c.secret))
	mac.Write([]byte(username))
	return username, base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func newHub() *Hub {
	adminRoom := os.Getenv("ROOMWS_ADMIN_ROOM")
	if adminRoom == "" {
		adminRoom = "admin-" + randomToken(8)
	}

	metricsToken := os.Getenv("ROOMWS_METRICS_TOKEN")
	if metricsToken == "" {
		metricsToken = randomToken(16)
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
		broadcast:    make(chan Message),
		register:     make(chan *Client),
		unregister:   make(chan *Client),
		clients:      make(map[*Client]bool),
		rooms:        make(map[string]map[*Client]bool),
		adminRoom:    adminRoom,
		whitelist:    allowed,
		historySize:  envInt("ROOMWS_HISTORY_SIZE", 0),
		history:      make(map[string][]Message),
		limiter:      lim,
		metricsToken: metricsToken,
		turn:         newTurnConfig(),
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
		slog.Error("marshal error in admin response", "error", err)
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
					slog.Error("marshal error for member_leave", "error", err)
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
				slog.Error("marshal error in broadcast", "error", err)
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

	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				slog.Warn("unexpected close", "error", err)
			}
			break
		}

		var msg Message
		if err := json.Unmarshal(message, &msg); err != nil {
			slog.Warn("unmarshal error", "error", err)
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
				slog.Error("marshal error in handshake", "error", err)
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
				slog.Error("marshal error for members list", "error", err)
				break
			}
			membersMsg := Message{
				Type:    "members",
				Room:    roomName,
				Message: json.RawMessage(membersData),
			}
			data, err := json.Marshal(membersMsg)
			if err != nil {
				slog.Error("marshal error for members message", "error", err)
				break
			}
			c.send <- data

			// Replay history oldest-first before notifying existing members.
			for _, hMsg := range histMsgs {
				d, err := json.Marshal(hMsg)
				if err != nil {
					slog.Error("marshal error for history message", "error", err)
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
				slog.Error("marshal error for member_join", "error", err)
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
					slog.Error("marshal error for subscribe callback", "error", err)
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
							slog.Error("marshal error for member_leave", "error", err)
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
			if c.pubLimiter != nil && !c.pubLimiter.Allow() {
				continue
			}
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
				slog.Warn("write error", "error", err)
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
		slog.Error("upgrade failed", "error", err)
		return
	}
	var pubLimiter *rate.Limiter
	if clientPublishRate > 0 {
		pubLimiter = rate.NewLimiter(rate.Limit(clientPublishRate), clientPublishBurst)
	}
	client := &Client{
		hub:        hub,
		conn:       conn,
		send:       make(chan []byte, 256),
		id:         generateID(),
		pubLimiter: pubLimiter,
	}
	client.hub.register <- client
	go client.writePump()
	go client.readPump()
}

// validMetricsToken checks the request's "Authorization: Bearer <token>" header
// against the configured metrics token using a constant-time comparison.
func validMetricsToken(token string, r *http.Request) bool {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	provided := strings.TrimPrefix(auth, prefix)
	return subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1
}

func healthCheck(port string) {
	resp, err := http.Get("http://localhost:" + port + "/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
	os.Exit(0)
}

// newMux builds the HTTP routes for the server. Extracted from main so it
// can be exercised directly in tests via httptest.
func newMux(hub *Hub) *http.ServeMux {
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
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if !validMetricsToken(hub.metricsToken, r) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		hub.mu.RLock()
		clients := len(hub.clients)
		rooms := len(hub.rooms)
		hub.mu.RUnlock()

		var m runtime.MemStats
		runtime.ReadMemStats(&m)

		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "# HELP roomws_uptime_seconds Time since server start in seconds.\n")
		fmt.Fprintf(w, "# TYPE roomws_uptime_seconds gauge\n")
		fmt.Fprintf(w, "roomws_uptime_seconds %f\n", time.Since(startTime).Seconds())
		fmt.Fprintf(w, "# HELP roomws_clients Number of connected clients.\n")
		fmt.Fprintf(w, "# TYPE roomws_clients gauge\n")
		fmt.Fprintf(w, "roomws_clients %d\n", clients)
		fmt.Fprintf(w, "# HELP roomws_rooms Number of active rooms.\n")
		fmt.Fprintf(w, "# TYPE roomws_rooms gauge\n")
		fmt.Fprintf(w, "roomws_rooms %d\n", rooms)
		fmt.Fprintf(w, "# HELP roomws_goroutines Number of goroutines.\n")
		fmt.Fprintf(w, "# TYPE roomws_goroutines gauge\n")
		fmt.Fprintf(w, "roomws_goroutines %d\n", runtime.NumGoroutine())
		fmt.Fprintf(w, "# HELP roomws_memory_bytes Allocated heap memory in bytes.\n")
		fmt.Fprintf(w, "# TYPE roomws_memory_bytes gauge\n")
		fmt.Fprintf(w, "roomws_memory_bytes %d\n", m.Alloc)
	})
	mux.HandleFunc("/turn", func(w http.ResponseWriter, r *http.Request) {
		if hub.turn == nil {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// The page is served from a different origin than this server, so the
		// fetch needs CORS. Reuse the WebSocket origin whitelist rather than
		// introducing a second list that can drift out of sync.
		if origin := r.Header.Get("Origin"); origin != "" {
			if !hub.isAllowed(origin) {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		username, credential := hub.turn.credentials(time.Now())
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"iceServers": []map[string]any{{
				"urls":       hub.turn.urls,
				"username":   username,
				"credential": credential,
			}},
			"ttl": int(hub.turn.ttl.Seconds()),
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
	return mux
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

	slog.Info("admin room configured", "room", hub.adminRoom)
	slog.Info("metrics token configured", "token", hub.metricsToken)

	server := &http.Server{
		Addr:    ":" + port,
		Handler: newMux(hub),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		slog.Info("shutting down server")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			slog.Error("shutdown error", "error", err)
		}
	}()

	slog.Info("server starting", "port", port)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("listen and serve failed", "error", err)
		os.Exit(1)
	}
	slog.Info("server stopped")
}
