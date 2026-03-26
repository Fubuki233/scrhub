# ScrHub

Web-based Android screen mirroring and remote control powered by the [scrcpy](https://github.com/Genymobile/scrcpy) protocol.  
Access your Android device from any browser on the LAN — no client installation required.

## Architecture

```
┌──────────┐    USB/TCP    ┌─────────────┐   WebSocket   ┌─────────┐
│  Android  │◄────────────►│   ScrHub     │◄─────────────►│ Browser │
│  Device   │   ADB        │  (Go server) │   HTTP/HTTPS  │ (Web UI)│
└──────────┘               └─────────────┘               └─────────┘
```

**ScrHub** pushes `scrcpy-server` to the Android device via ADB, starts an scrcpy session, and relays video/audio streams over WebSocket to browser clients. The browser decodes H.264 via WebCodecs API and sends touch/keyboard events back as scrcpy control messages.

## Features

- **Real-time screen mirroring** — H.264 + WebCodecs, low latency
- **Audio forwarding** — Opus codec
- **Full input support** — Mouse, touch, keyboard, scroll
- **Multi-device dashboard** — Manage multiple Android devices
- **Multi-viewer** — Multiple browsers watching simultaneously
- **Single binary** — Frontend embedded, zero external dependencies
- **HTTPS** — Auto-generated self-signed certificate
- **USB & WiFi** — Connect via USB or TCP/IP (including hotspot)

## Quick Start

### Download

Grab the latest release from [GitHub Releases](https://github.com/Fubuki233/scrhub/releases). Each package includes:
- `scrhub` — Go server binary
- `scrcpy-server` — scrcpy server JAR
- `adb` — Android Debug Bridge

### From Source

```bash
# Build
go build -trimpath -ldflags="-s -w" -o scrhub ./cmd/web-scrcpy/

# Run (requires adb and scrcpy-server in PATH or specified via flags)
./scrhub --listen :8080
```

### Usage

1. Connect your Android device via USB:
   ```bash
   adb devices
   ```

2. Start the server:
   ```bash
   ./scrhub --server-path ./scrcpy-server --adb ./adb
   ```

3. Open `http://<server-ip>:8080` in your browser.

4. Click **Connect** on your device, then **View** to open the player.

#### WiFi / Hotspot Mode

```bash
# Enable TCP/IP on device (requires initial USB connection)
adb tcpip 5555

# Then connect via IP in the web dashboard
# Or use the API:
curl http://localhost:8080/api/adb/connect -d '{"address":"192.168.1.100:5555"}'
```

## CLI Options

| Flag | Default | Description |
|------|---------|-------------|
| `--listen` | `:8080` | HTTP listen address |
| `--https` | `:8443` | HTTPS listen address (empty to disable) |
| `--server-path` | auto-detect | Path to scrcpy-server JAR |
| `--adb` | `adb` | Path to adb executable |
| `--max-size` | `1920` | Max video dimension (0 = native) |
| `--video-codec` | `h264` | Video codec (`h264`, `h265`, `av1`) |
| `--audio-codec` | `opus` | Audio codec (`opus`, `aac`, `flac`, `raw`) |
| `--bit-rate` | `8000000` | Video bitrate in bps |
| `--max-fps` | `60` | Max frames per second |

## API

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/devices` | List all devices |
| GET | `/api/defaults` | Get default streaming params |
| POST | `/api/devices/{serial}/connect` | Start scrcpy session |
| POST | `/api/devices/{serial}/disconnect` | Stop session |
| GET | `/api/devices/{serial}/status` | Session status |
| POST | `/api/devices/{serial}/tcpip` | Enable TCP/IP mode |
| POST | `/api/adb/connect` | Connect to device by IP |
| POST | `/api/adb/disconnect` | Disconnect TCP/IP device |
| WS | `/ws/device/{serial}` | WebSocket stream (video/audio/control) |

## Project Structure

```
├── cmd/web-scrcpy/main.go        # CLI entry point
├── frontend_embed.go              # Embeds frontend/ into binary
├── frontend/
│   ├── index.html                 # Device dashboard
│   ├── player.html                # Video player & controller
│   ├── css/style.css              # Dark theme
│   └── js/
│       ├── decoder.js             # WebCodecs H.264 decoder
│       ├── audio.js               # Opus audio decoder
│       ├── websocket.js           # WebSocket with channel demux
│       ├── protocol.js            # Control message serialization
│       ├── input.js               # Mouse/touch/keyboard handler
│       ├── mse-decoder.js         # MSE fallback decoder
│       └── mp4mux.js              # MP4 muxer for MSE
└── internal/
    ├── adb/adb.go                 # ADB CLI wrapper
    ├── protocol/                  # Packet parsing & control messages
    ├── device/
    │   ├── session.go             # scrcpy session lifecycle
    │   └── manager.go             # Multi-device management
    ├── relay/relay.go             # Stream relay & broadcast
    └── server/
        ├── server.go              # HTTP/WS routes
        └── tls.go                 # Self-signed TLS generation
```

## Browser Compatibility

| Browser | Video | Audio | Input |
|---------|-------|-------|-------|
| Chrome 94+ | ✅ | ✅ | ✅ |
| Edge 94+ | ✅ | ✅ | ✅ |
| Safari 16.4+ | ✅ | ✅ | ✅ |
| Firefox | ❌ (no WebCodecs) | ❌ | ✅ |

## VS Code Extension

Looking for a VS Code extension? Check out [Vscode-scrcpy](https://github.com/Fubuki233/Vscode-scrcpy) — control Android devices directly inside VS Code, powered by ScrHub.

## License

Apache License 2.0
