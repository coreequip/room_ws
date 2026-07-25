(function (root, factory) {
    if (typeof module === 'object' && module.exports) {
        module.exports = factory();
    } else if (typeof define === 'function' && define.amd) {
        define([], factory);
    } else {
        root.RoomWS = factory();
    }
}(typeof self !== 'undefined' ? self : this, function () {

class RoomWS {
    constructor(channelId, options = {}) {
        this.url = options.url || 'ws://localhost:8080';
        this.channelId = channelId;
        this.callbacks = new Map();
        this.callbackCount = 0;
        this.rooms = new Map();
        this.eventListeners = {};
        this.clientId = null;
        this.queue = [];

        this.reconnect = options.reconnect !== false;
        this.reconnectDelay = options.reconnectDelay || 500;
        this.maxReconnectDelay = options.maxReconnectDelay || 30000;
        this._reconnectAttempts = 0;
        this._closedByUser = false;

        this._connect();
    }

    _connect() {
        this.socket = new WebSocket(this.url);

        this.socket.onopen = () => {
            this._reconnectAttempts = 0;
            this._send({
                type: 'handshake',
                channel: this.channelId,
                version: 2,
                callback: this._addCallback((data) => {
                    this.clientId = data.client_id;
                    this._trigger('open', data.error);
                })
            });

            for (const roomName of this.rooms.keys()) {
                this._sendSubscribe(roomName);
            }

            this._flushQueue();
        };

        this.socket.onmessage = (event) => {
            let data;
            try {
                data = JSON.parse(event.data);
            } catch (err) {
                this._trigger('error', err);
                return;
            }
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

        this.socket.onclose = () => {
            this._trigger('close');
            if (this.reconnect && !this._closedByUser) {
                this._scheduleReconnect();
            }
        };
        this.socket.onerror = (err) => this._trigger('error', err);
    }

    _scheduleReconnect() {
        const delay = Math.min(
            this.reconnectDelay * Math.pow(2, this._reconnectAttempts),
            this.maxReconnectDelay
        );
        this._reconnectAttempts++;
        setTimeout(() => {
            if (!this._closedByUser) this._connect();
        }, delay);
    }

    close() {
        this._closedByUser = true;
        this.socket.close();
    }

    on(event, callback) {
        if (!this.eventListeners[event]) this.eventListeners[event] = [];
        this.eventListeners[event].push(callback);
    }

    off(event, callback) {
        if (!this.eventListeners[event]) return;
        if (callback === undefined) {
            delete this.eventListeners[event];
            return;
        }
        this.eventListeners[event] = this.eventListeners[event].filter(cb => cb !== callback);
    }

    _trigger(event, ...args) {
        if (this.eventListeners[event]) {
            this.eventListeners[event].forEach(cb => cb(...args));
        }
    }

    _send(data) {
        if (this.socket.readyState === WebSocket.OPEN) {
            this.socket.send(JSON.stringify(data));
        } else {
            this.queue.push(data);
        }
    }

    _flushQueue() {
        const pending = this.queue;
        this.queue = [];
        for (const data of pending) {
            this._send(data);
        }
    }

    _addCallback(cb) {
        const id = this.callbackCount++;
        this.callbacks.set(id, cb);
        return id;
    }

    _sendSubscribe(roomName, onOpen) {
        this._send({
            type: 'subscribe',
            room: roomName,
            callback: this._addCallback(() => {
                if (onOpen) onOpen();
                const room = this.rooms.get(roomName);
                if (room) room._trigger('open');
            })
        });
    }

    subscribe(roomName) {
        let room = this.rooms.get(roomName);
        if (!room) {
            room = new Room(this, roomName);
            this.rooms.set(roomName, room);
        }
        this._sendSubscribe(roomName);
        return room;
    }

    unsubscribe(roomName) {
        this.rooms.delete(roomName);
        this._send({
            type: 'unsubscribe',
            room: roomName
        });
    }

    publish({room, message, no_echo}) {
        this._send({
            type: 'publish',
            room,
            message,
            no_echo
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

    off(event, callback) {
        if (!this.eventListeners[event]) return;
        if (callback === undefined) {
            delete this.eventListeners[event];
            return;
        }
        this.eventListeners[event] = this.eventListeners[event].filter(cb => cb !== callback);
    }

    _trigger(event, ...args) {
        if (this.eventListeners[event]) {
            this.eventListeners[event].forEach(cb => cb(...args));
        }
    }

    unsubscribe() {
        this.drone.unsubscribe(this.name);
    }
}

return RoomWS;

}));
