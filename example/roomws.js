class RoomWS {
    constructor(channelId, options = {}) {
        const url = options.url || 'ws://localhost:8080';
        this.socket = new WebSocket(url);
        this.channelId = channelId;
        this.callbacks = new Map();
        this.callbackCount = 0;
        this.rooms = new Map();
        this.eventListeners = {};
        this.clientId = null;

        this.socket.onopen = () => {
            this._send({
                type: 'handshake',
                channel: this.channelId,
                version: 2,
                callback: this._addCallback((data) => {
                    this.clientId = data.client_id;
                    this._trigger('open', data.error);
                })
            });
        };

        this.socket.onmessage = (event) => {
            const data = JSON.parse(event.data);
            if (data.callback !== undefined) {
                const cb = this.callbacks.get(data.callback);
                if (cb) {
                    cb(data);
                    this.callbacks.delete(data.callback);
                }
            } else if (data.type === 'publish') {
                const room = this.rooms.get(data.room);
                if (room) {
                    room._trigger('message', data.message, data);
                }
            } else if (data.type === 'members' || data.type === 'member_join' || data.type === 'member_leave') {
                const room = this.rooms.get(data.room);
                if (room) {
                    const type = data.type;
                    if (type === 'members') {
                        room.members = data.message;
                        room._trigger('members', data.message, data);
                    } else if (type === 'member_join') {
                        room.members.push(data.client_id);
                        room._trigger('member_join', data.client_id, data);
                        room._trigger('join', data.client_id, data); // Alias for convenience
                    } else if (type === 'member_leave') {
                        room.members = room.members.filter(id => id !== data.client_id);
                        room._trigger('member_leave', data.client_id, data);
                        room._trigger('leave', data.client_id, data); // Alias for convenience
                    }
                }
            }
        };

        this.socket.onclose = () => this._trigger('close');
        this.socket.onerror = (err) => this._trigger('error', err);
    }

    on(event, callback) {
        if (!this.eventListeners[event]) this.eventListeners[event] = [];
        this.eventListeners[event].push(callback);
    }

    _trigger(event, ...args) {
        if (this.eventListeners[event]) {
            this.eventListeners[event].forEach(cb => cb(...args));
        }
    }

    _send(data) {
        if (this.socket.readyState === WebSocket.OPEN) {
            this.socket.send(JSON.stringify(data));
        }
    }

    _addCallback(cb) {
        const id = this.callbackCount++;
        this.callbacks.set(id, cb);
        return id;
    }

    subscribe(roomName) {
        const room = new Room(this, roomName);
        this.rooms.set(roomName, room);
        this._send({
            type: 'subscribe',
            room: roomName,
            callback: this._addCallback(() => {
                room._trigger('open');
            })
        });
        return room;
    }

    publish({room, message}) {
        this._send({
            type: 'publish',
            room,
            message
        });
    }
}

class Room {
    constructor(drone, name) {
        this.drone = drone;
        this.name = name;
        this.eventListeners = {};
        this.members = [];
    }

    on(event, callback) {
        if (!this.eventListeners[event]) this.eventListeners[event] = [];
        this.eventListeners[event].push(callback);
    }

    _trigger(event, ...args) {
        if (this.eventListeners[event]) {
            this.eventListeners[event].forEach(cb => cb(...args));
        }
    }
}
