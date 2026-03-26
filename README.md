# Web scrcpy

A web-based remote viewer and controller for Android devices using scrcpy protocol. Access your Android device from any browser on the LAN — no client installation required.

## Architecture

```
┌──────────┐    USB/TCP    ┌───────────────┐   WebSocket   ┌─────────┐
│  Android  │◄────────────►│  web-scrcpy   │◄─────────────►│ Browser │
│  Device   │   ADB        │  (Go server)  │   HTTP        │ (Web UI)│
└──────────┘               └───────────────┘               └─────────┘
```

- **Go server** pushes `scrcpy-server.jar` to the Android device via ADB, starts an scrcpy session, and relays the video/audio streams over WebSocket to browser clients.
- **Browser** decodes H.264 video via WebCodecs API and renders to canvas. Touch/mouse/keyboard events are captured and sent back as scrcpy control messages.
- **Multi-viewer**: Multiple browsers can watch simultaneously. One viewer at a time can request interactive control.

## Features

- Real-time screen mirroring via H.264 + WebCodecs
- Audio forwarding (Opus)
- Mouse, touch, and keyboard input
- Multi-device management dashboard
- Multiple concurrent viewers (single controller)
- Embedded frontend — single binary deployment
- Dark theme UI

## Requirements

- Go 1.22+
- ADB installed and in PATH
- `scrcpy-server` JAR file (from [scrcpy releases](https://github.com/Genymobile/scrcpy/releases), version 3.3.4)
- Android device connected via USB or TCP/IP with USB debugging enabled
- Browser with [WebCodecs API](https://caniuse.com/webcodecs) support (Chrome 94+, Edge 94+)

## Build

```bash
cd web
go build -o web-scrcpy ./cmd/web-scrcpy/
```

## Usage

1. Connect your Android device via USB and verify it appears:
   ```bash
   adb devices
   ```

2. Download or locate your `scrcpy-server` file (e.g., `scrcpy-server-v3.3.4`).

3. Start the server:
   ```bash
   ./web-scrcpy --server-path /path/to/scrcpy-server --listen :8080
   ```

4. Open `http://<server-ip>:8080` in your browser.

5. Click **Connect** on your device, then **View** to open the player.

## CLI Options

| Flag | Default | Description |
|------|---------|-------------|
| `--listen` | `:8080` | HTTP listen address |
| `--server-path` | `scrcpy-server` | Path to scrcpy-server JAR |
| `--adb` | `adb` | Path to adb executable |
| `--max-size` | `1024` | Max video resolution dimension |
| `--video-codec` | `h264` | Video codec (h264, h265, av1) |
| `--audio-codec` | `opus` | Audio codec (opus, aac, flac, raw) |
| `--bit-rate` | `4000000` | Video bitrate in bps |
| `--max-fps` | `30` | Max frames per second |

## Project Structure

```
web/
├── cmd/web-scrcpy/main.go       # Entry point, CLI flags, graceful shutdown
├── frontend_embed.go             # Embeds frontend/ into binary
├── frontend/
│   ├── index.html                # Device list dashboard
│   ├── player.html               # Video player / controller
│   ├── css/style.css             # Dark theme styles
│   └── js/
│       ├── websocket.js          # WebSocket client with channel demuxing
│       ├── decoder.js            # WebCodecs H.264 video decoder
│       ├── audio.js              # WebCodecs Opus audio decoder
│       ├── protocol.js           # Binary control message serialization
│       └── input.js              # Mouse/touch/keyboard event handler
└── internal/
    ├── adb/adb.go                # ADB CLI wrapper
    ├── protocol/
    │   ├── packet.go             # Stream packet parsing (codec IDs, headers)
    │   ├── control.go            # Control message validation
    │   └── device.go             # Device message parsing
    ├── device/
    │   ├── session.go            # Full scrcpy session lifecycle
    │   └── manager.go            # Multi-device session management
    ├── relay/relay.go            # TCP→WebSocket stream relay + broadcast
    └── server/server.go          # HTTP routes + WebSocket handler
```

## Browser Compatibility

| Browser | Video | Audio | Input |
|---------|-------|-------|-------|
| Chrome 94+ | ✅ | ✅ | ✅ |
| Edge 94+ | ✅ | ✅ | ✅ |
| Safari 16.4+ | ✅ | ✅ | ✅ |
| Firefox | ❌ (no WebCodecs) | ❌ | ✅ |

## License

Same as scrcpy — Apache License 2.0
