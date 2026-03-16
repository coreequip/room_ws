<p align="center">
  <img src="web/icon.svg" width="128" height="128" alt="ROOM.WS Logo">
</p>

# ROOM.WS

A lightweight, self-hosted real-time messaging server using the Pub/Sub pattern. Built with Go and WebSockets for low-latency communication.

## Server Setup

The server is located in the `./server` directory.

```bash
cd server
go run main.go
```

### Protocol
ROOM.WS uses a simple JSON-based Pub/Sub protocol. It supports rooms, presence (join/leave events), and persistent connections.

### Environment Variables
- `ROOMWS_ADMIN_ROOM`: Name of the administrative room (default: generated).
- `ROOMWS_ALLOWED_ORIGINS`: Comma-separated list of allowed origins (e.g., `room.ws, localhost`).

## Using the Client (`roomws.js`)

The client library provides a simple interface to interact with the server.

### 1. Initialize Connection

```javascript
const drone = new RoomWS('your-channel-id', {
  url: 'ws://localhost:8080'
});

drone.on('open', error => {
  if (error) return console.error(error);
  console.log('Connected with client ID:', drone.clientId);
});
```

### 2. Subscribe to a Room

```javascript
const room = drone.subscribe('lobby');

room.on('open', () => {
  console.log('Successfully joined lobby');
});

// Receive messages
room.on('message', (message, data) => {
  console.log('Received:', message, 'from', data.client_id);
});
```

### 3. Presence (Member Events)

```javascript
room.on('members', members => {
  console.log('Current members:', members);
});

room.on('member_join', memberId => {
  console.log('User joined:', memberId);
});

room.on('member_leave', memberId => {
  console.log('User left:', memberId);
});
```

### 4. Publish Messages

```javascript
drone.publish({
  room: 'lobby',
  message: { text: 'Hello World!' }
});
```

## Admin Commands

If you are in the admin room, you can send commands as messages:
- `status`: Get server uptime, memory, and client count.
- `add <domain>`: Whitelist a new origin.
- `remove <domain>`: Remove a domain from the whitelist.
- `list`: Show all whitelisted domains.

## License
MIT
