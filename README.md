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
- `ROOMWS_PORT`: Port the server listens on (default: `8080`).
- `ROOMWS_ADMIN_ROOM`: Name of the administrative room (default: randomly generated).
- `ROOMWS_ALLOWED_ORIGINS`: Comma-separated list of allowed origins (e.g., `room.ws, localhost`). `localhost` and `127.0.0.1` are always allowed.
- `ROOMWS_WRITE_WAIT`: Timeout for writing a message to a client (default: `10s`).
- `ROOMWS_PONG_WAIT`: Timeout for receiving a pong from a client before disconnecting (default: `60s`). The ping interval is derived as 90% of this value.
- `ROOMWS_RATE_LIMIT`: Max new WebSocket connections per second per IP (default: `5`). Set to `0` to disable.
- `ROOMWS_RATE_BURST`: Burst size for the rate limiter (default: `10`).
- `ROOMWS_HISTORY_SIZE`: Number of messages to retain per room for replay on subscribe (default: `0` = disabled).

## Health Check

The server exposes a `/health` endpoint that returns `200 OK` with a JSON body:

```json
{ "status": "ok", "uptime": "5m30s", "clients": 3, "rooms": 1 }
```

The Docker image uses this endpoint as its built-in `HEALTHCHECK`.

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

By default the server echoes published messages back to the sender (so you receive the server-assigned `id` and `timestamp`). Pass `no_echo: true` to suppress the echo, e.g. when you display your own messages optimistically:

```javascript
drone.publish({
  room: 'lobby',
  message: { text: 'Hello World!' },
  no_echo: true
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
