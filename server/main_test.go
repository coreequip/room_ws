package main

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestGenerateID(t *testing.T) {
	id1 := generateID()
	id2 := generateID()

	if len(id1) != 16 {
		t.Errorf("Expected ID length 16, got %d", len(id1))
	}
	if id1 == id2 {
		t.Errorf("Expected unique IDs, got same: %s", id1)
	}
}

func TestHubIsAllowed(t *testing.T) {
	h := newHub()

	tests := []struct {
		origin   string
		expected bool
	}{
		{"http://localhost:3000", true},
		{"http://127.0.0.1:8080", true},
		{"https://room.ws", false}, // Not in default whitelist
		{"", true},                 // Direct connection
		{"invalid-url", false},
	}

	for _, tt := range tests {
		if got := h.isAllowed(tt.origin); got != tt.expected {
			t.Errorf("isAllowed(%q) = %v; want %v", tt.origin, got, tt.expected)
		}
	}

	// Test with environment variable origins
	os.Setenv("ROOMWS_ALLOWED_ORIGINS", "example.com, test.org")
	h2 := newHub()
	if !h2.isAllowed("https://example.com") {
		t.Error("Expected example.com to be allowed")
	}
	if !h2.isAllowed("https://sub.example.com") {
		t.Error("Expected sub.example.com to be allowed via suffix check")
	}
}

func TestHubAdminCommands(t *testing.T) {
	h := newHub()
	// Use buffered channel to prevent blocking as hub.run() is not active here
	h.broadcast = make(chan Message, 10)

	// Test 'add' command
	h.handleAdminCommand("add room.ws")
	if !h.isAllowed("https://room.ws") {
		t.Error("Expected room.ws to be whitelisted after 'add' command")
	}

	// Ensure no panic
	h.handleAdminCommand("list")

	// Test 'remove' command
	h.handleAdminCommand("remove room.ws")
	if h.isAllowed("https://room.ws") {
		t.Error("Expected room.ws to be removed from whitelist")
	}

	// Test protected removal
	h.handleAdminCommand("remove localhost")
	if !h.isAllowed("http://localhost") {
		t.Error("Expected localhost to remain whitelisted (protected)")
	}
}

func TestMessageJSON(t *testing.T) {
	raw := `{"type":"publish","room":"general","message":"hello"}`
	var msg Message
	err := json.Unmarshal([]byte(raw), &msg)
	if err != nil {
		t.Fatalf("Failed to unmarshal message: %v", err)
	}

	if msg.Type != "publish" || msg.Room != "general" {
		t.Errorf("Unexpected message content: %+v", msg)
	}

	var content string
	err = json.Unmarshal(msg.Message, &content)
	if err != nil {
		t.Fatalf("Failed to unmarshal raw message content: %v", err)
	}
	if content != "hello" {
		t.Errorf("Expected 'hello', got %s", content)
	}
}

func TestMessageNoEcho(t *testing.T) {
	raw := `{"type":"publish","room":"general","message":"hello","no_echo":true}`
	var msg Message
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}
	if !msg.NoEcho {
		t.Error("Expected NoEcho to be true")
	}

	// no_echo must not appear in outgoing broadcast (NoEcho is false on publishMsg)
	publishMsg := Message{
		Type:     "publish",
		Room:     msg.Room,
		Message:  msg.Message,
		ClientID: "abc",
	}
	data, err := json.Marshal(publishMsg)
	if err != nil {
		t.Fatalf("Failed to marshal: %v", err)
	}
	if strings.Contains(string(data), "no_echo") {
		t.Errorf("no_echo must not appear in outgoing message: %s", data)
	}
}

func TestEnvFloatNegative(t *testing.T) {
	os.Setenv("ROOMWS_TEST_FLOAT", "-3.5")
	defer os.Unsetenv("ROOMWS_TEST_FLOAT")

	if got := envFloat("ROOMWS_TEST_FLOAT", 5); got != 5 {
		t.Errorf("expected fallback to default 5 for negative value, got %v", got)
	}
}

func TestEnvFloatInvalid(t *testing.T) {
	os.Setenv("ROOMWS_TEST_FLOAT", "not-a-number")
	defer os.Unsetenv("ROOMWS_TEST_FLOAT")

	if got := envFloat("ROOMWS_TEST_FLOAT", 5); got != 5 {
		t.Errorf("expected fallback to default 5 for invalid value, got %v", got)
	}
}

func TestServeWsMessageSizeLimit(t *testing.T) {
	hub := newHub()
	go hub.run()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveWs(hub, w, r)
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	oversized := make([]byte, maxMessageSize+1024)
	for i := range oversized {
		oversized[i] = 'a'
	}
	if err := conn.WriteMessage(websocket.TextMessage, oversized); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Error("expected server to close the connection after an oversized message")
	}
}

func TestMetricsEndpointRequiresToken(t *testing.T) {
	hub := newHub()
	go hub.run()

	srv := httptest.NewServer(newMux(hub))
	defer srv.Close()

	// No Authorization header at all.
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 without token, got %d", resp.StatusCode)
	}

	// Wrong token.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 with wrong token, got %d", resp.StatusCode)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	hub := newHub()
	go hub.run()

	srv := httptest.NewServer(newMux(hub))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+hub.metricsToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with valid token, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read body: %v", err)
	}
	for _, want := range []string{"roomws_uptime_seconds", "roomws_clients", "roomws_rooms", "roomws_goroutines", "roomws_memory_bytes"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("expected metrics body to contain %q, got: %s", want, body)
		}
	}
}

func TestHubNew(t *testing.T) {
	customAdmin := "secret-admin-room"
	os.Setenv("ROOMWS_ADMIN_ROOM", customAdmin)
	defer os.Unsetenv("ROOMWS_ADMIN_ROOM")

	h := newHub()
	if h.adminRoom != customAdmin {
		t.Errorf("Expected admin room %s, got %s", customAdmin, h.adminRoom)
	}

	if !strings.HasPrefix(h.adminRoom, "secret-admin-room") {
		t.Error("Admin room prefix mismatch")
	}
}

func TestHubMetricsTokenGenerated(t *testing.T) {
	h1 := newHub()
	h2 := newHub()

	if h1.metricsToken == "" {
		t.Error("expected a generated metrics token, got empty string")
	}
	if h1.metricsToken == h2.metricsToken {
		t.Error("expected generated metrics tokens to be unique per hub")
	}
}

func TestHubMetricsTokenFromEnv(t *testing.T) {
	customToken := "secret-metrics-token"
	os.Setenv("ROOMWS_METRICS_TOKEN", customToken)
	defer os.Unsetenv("ROOMWS_METRICS_TOKEN")

	h := newHub()
	if h.metricsToken != customToken {
		t.Errorf("expected metrics token %q, got %q", customToken, h.metricsToken)
	}
}

func TestTurnEndpointReturnsHMACCredentials(t *testing.T) {
	os.Setenv("ROOMWS_TURN_SECRET", "s3cr3t")
	os.Setenv("ROOMWS_TURN_URLS", "turns:turn.room.ws:443?transport=tcp,turn:turn.room.ws:3478")
	os.Setenv("ROOMWS_TURN_TTL", "2h")
	defer func() {
		os.Unsetenv("ROOMWS_TURN_SECRET")
		os.Unsetenv("ROOMWS_TURN_URLS")
		os.Unsetenv("ROOMWS_TURN_TTL")
	}()

	h := newHub()
	srv := httptest.NewServer(newMux(h))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/turn")
	if err != nil {
		t.Fatalf("GET /turn: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200, got %d", resp.StatusCode)
	}

	var got struct {
		IceServers []struct {
			URLs       []string `json:"urls"`
			Username   string   `json:"username"`
			Credential string   `json:"credential"`
		} `json:"iceServers"`
		TTL int `json:"ttl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(got.IceServers) != 1 {
		t.Fatalf("Expected 1 ice server entry, got %d", len(got.IceServers))
	}
	entry := got.IceServers[0]

	want := []string{"turns:turn.room.ws:443?transport=tcp", "turn:turn.room.ws:3478"}
	if len(entry.URLs) != len(want) {
		t.Fatalf("Expected %d urls, got %v", len(want), entry.URLs)
	}
	for i, u := range want {
		if entry.URLs[i] != u {
			t.Errorf("urls[%d] = %q; want %q", i, entry.URLs[i], u)
		}
	}

	// coturn REST API: username is "<unix-expiry>:<name>", credential is
	// base64(HMAC-SHA1(secret, username)).
	expiry, name, found := strings.Cut(entry.Username, ":")
	if !found || name == "" {
		t.Fatalf("username %q is not in <expiry>:<name> form", entry.Username)
	}
	ts, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil {
		t.Fatalf("username expiry %q is not a unix timestamp: %v", expiry, err)
	}
	if delta := time.Until(time.Unix(ts, 0)) - 2*time.Hour; delta > time.Minute || delta < -time.Minute {
		t.Errorf("expiry is %v off from the configured TTL", delta)
	}

	mac := hmac.New(sha1.New, []byte("s3cr3t"))
	mac.Write([]byte(entry.Username))
	wantCred := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if entry.Credential != wantCred {
		t.Errorf("credential = %q; want %q", entry.Credential, wantCred)
	}

	if got.TTL != int((2 * time.Hour).Seconds()) {
		t.Errorf("ttl = %d; want %d", got.TTL, int((2 * time.Hour).Seconds()))
	}
}

func TestTurnEndpointDisabledWithoutSecret(t *testing.T) {
	os.Unsetenv("ROOMWS_TURN_SECRET")

	h := newHub()
	srv := httptest.NewServer(newMux(h))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/turn")
	if err != nil {
		t.Fatalf("GET /turn: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("Expected 404 when no TURN secret is configured, got %d", resp.StatusCode)
	}
}

func TestTurnEndpointAllowsConfiguredOrigin(t *testing.T) {
	os.Setenv("ROOMWS_TURN_SECRET", "s3cr3t")
	os.Setenv("ROOMWS_ALLOWED_ORIGINS", "share.room.ws")
	defer func() {
		os.Unsetenv("ROOMWS_TURN_SECRET")
		os.Unsetenv("ROOMWS_ALLOWED_ORIGINS")
	}()

	h := newHub()
	srv := httptest.NewServer(newMux(h))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/turn", nil)
	req.Header.Set("Origin", "https://share.room.ws")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /turn: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://share.room.ws" {
		t.Errorf("Access-Control-Allow-Origin = %q; want the request origin", got)
	}
	if got := resp.Header.Get("Vary"); !strings.Contains(got, "Origin") {
		t.Errorf("Vary = %q; want it to contain Origin", got)
	}
}

func TestTurnEndpointRejectsForeignOrigin(t *testing.T) {
	os.Setenv("ROOMWS_TURN_SECRET", "s3cr3t")
	defer os.Unsetenv("ROOMWS_TURN_SECRET")

	h := newHub()
	srv := httptest.NewServer(newMux(h))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/turn", nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /turn: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("Expected 403 for a foreign origin, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Expected no CORS header for a rejected origin, got %q", got)
	}
}

func TestTurnEndpointRejectsNonGET(t *testing.T) {
	os.Setenv("ROOMWS_TURN_SECRET", "s3cr3t")
	defer os.Unsetenv("ROOMWS_TURN_SECRET")

	h := newHub()
	srv := httptest.NewServer(newMux(h))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/turn", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST /turn: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("Expected 405, got %d", resp.StatusCode)
	}
}
