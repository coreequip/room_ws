package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
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
