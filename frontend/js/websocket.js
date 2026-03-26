/**
 * WebSocket client with auto-reconnection for web-scrcpy.
 * Binary protocol: [1 byte channel][payload]
 */

const CHANNEL_VIDEO = 0x01;
const CHANNEL_AUDIO = 0x02;
const CHANNEL_CONTROL = 0x03;
const CHANNEL_DEVICE = 0x04;
const CHANNEL_MGMT = 0x10;

class ScrcpyClient {
    constructor(serial) {
        this.serial = serial;
        this.ws = null;
        this.connected = false;
        this.reconnectDelay = 1000;
        this.maxReconnectDelay = 10000;
        this.shouldReconnect = true;

        // Callbacks
        this.onVideoData = null;    // (rawPacket: Uint8Array) => void
        this.onAudioData = null;    // (rawPacket: Uint8Array) => void
        this.onDeviceMsg = null;    // (rawMsg: Uint8Array) => void
        this.onMgmtMsg = null;     // (json: object) => void
        this.onConnected = null;    // () => void
        this.onDisconnected = null; // () => void
    }

    connect() {
        const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
        const url = `${protocol}//${location.host}/ws/device/${encodeURIComponent(this.serial)}`;

        this.ws = new WebSocket(url);
        this.ws.binaryType = 'arraybuffer';

        this.ws.onopen = () => {
            this.connected = true;
            this.reconnectDelay = 1000;
            if (this.onConnected) this.onConnected();
        };

        this.ws.onclose = () => {
            this.connected = false;
            if (this.onDisconnected) this.onDisconnected();
            if (this.shouldReconnect) {
                setTimeout(() => this.connect(), this.reconnectDelay);
                this.reconnectDelay = Math.min(this.reconnectDelay * 1.5, this.maxReconnectDelay);
            }
        };

        this.ws.onerror = () => {
            // onclose will fire after this
        };

        this.ws.onmessage = (event) => {
            const data = new Uint8Array(event.data);
            if (data.length < 2) return;

            const channel = data[0];
            const payload = data.subarray(1);

            switch (channel) {
                case CHANNEL_VIDEO:
                    if (this.onVideoData) this.onVideoData(payload);
                    break;
                case CHANNEL_AUDIO:
                    if (this.onAudioData) this.onAudioData(payload);
                    break;
                case CHANNEL_DEVICE:
                    if (this.onDeviceMsg) this.onDeviceMsg(payload);
                    break;
                case CHANNEL_MGMT:
                    try {
                        const json = JSON.parse(new TextDecoder().decode(payload));
                        if (this.onMgmtMsg) this.onMgmtMsg(json);
                    } catch (e) { /* ignore */ }
                    break;
            }
        };
    }

    sendControl(data) {
        if (!this.connected) return;
        const frame = new Uint8Array(1 + data.length);
        frame[0] = CHANNEL_CONTROL;
        frame.set(data, 1);
        this.ws.send(frame.buffer);
    }

    sendMgmt(obj) {
        if (!this.connected) return;
        const json = new TextEncoder().encode(JSON.stringify(obj));
        const frame = new Uint8Array(1 + json.length);
        frame[0] = CHANNEL_MGMT;
        frame.set(json, 1);
        this.ws.send(frame.buffer);
    }

    requestControl() {
        this.sendMgmt({ type: 'request_control' });
    }

    releaseControl() {
        this.sendMgmt({ type: 'release_control' });
    }

    disconnect() {
        this.shouldReconnect = false;
        if (this.ws) {
            this.ws.close();
        }
    }

    /** Cleanly close and reconnect (used after settings change). */
    reconnect() {
        this.shouldReconnect = false;
        if (this.ws) {
            this.ws.close();
        }
        // Re-enable auto-reconnect and connect after a short delay
        setTimeout(() => {
            this.shouldReconnect = true;
            this.reconnectDelay = 1000;
            this.connect();
        }, 500);
    }
}

export { ScrcpyClient, CHANNEL_VIDEO, CHANNEL_AUDIO, CHANNEL_CONTROL, CHANNEL_DEVICE, CHANNEL_MGMT };
