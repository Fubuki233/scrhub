package relay

import (
	"io"
	"log"
	"sync"
	"time"

	"github.com/Fubuki233/scrhub/internal/device"
	"github.com/Fubuki233/scrhub/internal/protocol"
	"github.com/gorilla/websocket"
)

// WebSocket channel prefixes for multiplexing
const (
	ChannelVideo   = byte(0x01)
	ChannelAudio   = byte(0x02)
	ChannelControl = byte(0x03)
	ChannelDevice  = byte(0x04)
	ChannelMgmt    = byte(0x10)
)

// Client represents a connected WebSocket viewer.
type Client struct {
	ID   string
	Conn *websocket.Conn
	send chan []byte

	mu          sync.Mutex
	open        bool
	videoSynced bool // false → drop P-frames until next keyframe
}

// NewClient creates a new client.
func NewClient(id string, conn *websocket.Conn) *Client {
	// Disable compression and enable TCP no-delay for low latency
	conn.EnableWriteCompression(false)
	rawConn := conn.UnderlyingConn()
	if tc, ok := rawConn.(interface{ SetNoDelay(bool) error }); ok {
		tc.SetNoDelay(true)
	}
	return &Client{
		ID:          id,
		Conn:        conn,
		send:        make(chan []byte, 64),
		open:        true,
		videoSynced: true,
	}
}

// Send queues a message for sending. Drops if buffer is full.
func (c *Client) Send(msg []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.open {
		return
	}
	select {
	case c.send <- msg:
	default:
		// Drop frame if client is too slow
	}
}

// SendVideo sends a video frame with sync-aware dropping.
// When the send buffer is full, the client becomes "out of sync" and
// ALL subsequent P-frames are dropped until the next keyframe arrives.
// This prevents partial decode chains that cause MSE to freeze.
func (c *Client) SendVideo(msg []byte, isConfig, isKeyFrame bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.open {
		return
	}

	// Config packets are always sent (SPS/PPS)
	if isConfig {
		select {
		case c.send <- msg:
		default:
		}
		return
	}

	// If out of sync, drop everything except keyframes
	if !c.videoSynced && !isKeyFrame {
		return
	}

	select {
	case c.send <- msg:
		// Keyframes restore sync
		if isKeyFrame {
			c.videoSynced = true
		}
	default:
		// Buffer full → mark client as out of sync
		c.videoSynced = false
	}
}

// Close closes the client.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.open {
		return
	}
	c.open = false
	close(c.send)
}

// WritePump sends queued messages to the WebSocket with write coalescing.
// Multiple queued messages are batched into a single WebSocket write to
// reduce syscall overhead and TCP small-packet latency.
func (c *Client) WritePump() {
	defer c.Conn.Close()
	for msg := range c.send {
		c.Conn.SetWriteDeadline(time.Now().Add(5 * time.Second))

		w, err := c.Conn.NextWriter(websocket.BinaryMessage)
		if err != nil {
			return
		}
		w.Write(msg)
		w.Close()

		// Drain all immediately available messages
		n := len(c.send)
		for i := 0; i < n; i++ {
			extra := <-c.send
			c.Conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			w2, err := c.Conn.NextWriter(websocket.BinaryMessage)
			if err != nil {
				return
			}
			w2.Write(extra)
			w2.Close()
		}

		// Flush the underlying TCP buffer
		if flusher, ok := c.Conn.UnderlyingConn().(interface{ Flush() error }); ok {
			flusher.Flush()
		}
	}
}

// Relay manages streaming from a device session to multiple WebSocket clients.
type Relay struct {
	session *device.Session

	mu         sync.RWMutex
	clients    map[string]*Client
	controller string // client ID that holds control

	// Cached data for new clients
	videoConfig  []byte // last SPS/PPS config packet (with channel prefix)
	lastKeyFrame []byte // last keyframe (with channel prefix)
	audioConfig  []byte // audio config if any

	stopCh chan struct{}
	once   sync.Once
}

// NewRelay creates a relay for a device session.
func NewRelay(session *device.Session) *Relay {
	return &Relay{
		session: session,
		clients: make(map[string]*Client),
		stopCh:  make(chan struct{}),
	}
}

// Start begins reading from the device and relaying to clients.
func (r *Relay) Start() {
	go r.relayVideoStream()
	go r.relayAudioStream()
	go r.relayDeviceMessages()
}

// Stop stops the relay.
func (r *Relay) Stop() {
	r.once.Do(func() {
		close(r.stopCh)
		r.mu.Lock()
		for _, c := range r.clients {
			c.Close()
		}
		r.clients = make(map[string]*Client)
		r.mu.Unlock()
	})
}

// AddClient registers a new viewer.
func (r *Relay) AddClient(client *Client) {
	r.mu.Lock()
	r.clients[client.ID] = client
	count := len(r.clients)

	// Send cached video config + keyframe so client can start decoding immediately
	videoConfig := r.videoConfig
	lastKeyFrame := r.lastKeyFrame
	audioConfig := r.audioConfig
	r.mu.Unlock()

	if videoConfig != nil {
		client.Send(videoConfig)
	}
	if lastKeyFrame != nil {
		client.Send(lastKeyFrame)
	}
	if audioConfig != nil {
		client.Send(audioConfig)
	}

	log.Printf("[%s] client %s joined (total: %d)", r.session.Serial, client.ID, count)
	r.broadcastViewerCount()
}

// RemoveClient unregisters a viewer.
func (r *Relay) RemoveClient(clientID string) {
	r.mu.Lock()
	client, ok := r.clients[clientID]
	if ok {
		delete(r.clients, clientID)
		client.Close()
	}
	// Release control if this client held it
	if r.controller == clientID {
		r.controller = ""
	}
	count := len(r.clients)
	r.mu.Unlock()

	if ok {
		log.Printf("[%s] client %s left (total: %d)", r.session.Serial, clientID, count)
		r.broadcastViewerCount()
	}
}

// RequestControl attempts to grant control to a client.
func (r *Relay) RequestControl(clientID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.controller == "" || r.controller == clientID {
		r.controller = clientID
		return true
	}
	return false
}

// ReleaseControl releases control from a client.
func (r *Relay) ReleaseControl(clientID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.controller == clientID {
		r.controller = ""
	}
}

// IsController checks if a client holds control.
func (r *Relay) IsController(clientID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.controller == clientID
}

// HandleControlMsg processes a control message from a client.
func (r *Relay) HandleControlMsg(clientID string, data []byte) {
	if !r.IsController(clientID) {
		return
	}
	if err := r.session.SendControlMsg(data); err != nil {
		log.Printf("[%s] error sending control msg: %v", r.session.Serial, err)
	}
}

// ClientCount returns the number of connected clients.
func (r *Relay) ClientCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.clients)
}

func (r *Relay) broadcast(msg []byte) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, c := range r.clients {
		c.Send(msg)
	}
}

// broadcastVideo sends a video frame to all clients with sync-aware dropping.
func (r *Relay) broadcastVideo(msg []byte, isConfig, isKeyFrame bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, c := range r.clients {
		c.SendVideo(msg, isConfig, isKeyFrame)
	}
}

func (r *Relay) broadcastViewerCount() {
	count := r.ClientCount()
	msg := buildMgmtMsg(map[string]interface{}{
		"type":  "viewer_count",
		"count": count,
	})
	r.broadcast(msg)
}

func (r *Relay) relayVideoStream() {
	defer r.Stop()

	conn := r.session.VideoConn
	pktCount := 0
	for {
		pkt, raw, err := protocol.ReadPacket(conn)
		if err != nil {
			if err != io.EOF {
				log.Printf("[%s] video stream error: %v", r.session.Serial, err)
			}
			log.Printf("[%s] video stream ended after %d packets", r.session.Serial, pktCount)
			return
		}

		// Check stop after read (non-blocking read naturally blocks on conn)
		select {
		case <-r.stopCh:
			return
		default:
		}

		pktCount++
		if pktCount <= 5 || pktCount%500 == 0 {
			log.Printf("[%s] video pkt #%d: config=%v key=%v size=%d",
				r.session.Serial, pktCount, pkt.IsConfig, pkt.IsKeyFrame, len(pkt.Data))
		}

		// Prepend channel byte — reuse raw's backing array if possible
		frame := make([]byte, 1+len(raw))
		frame[0] = ChannelVideo
		copy(frame[1:], raw)

		// Cache config and keyframes for new clients
		if pkt.IsConfig || pkt.IsKeyFrame {
			r.mu.Lock()
			if pkt.IsConfig {
				r.videoConfig = frame
			}
			if pkt.IsKeyFrame {
				r.lastKeyFrame = frame
			}
			r.mu.Unlock()
		}

		r.broadcastVideo(frame, pkt.IsConfig, pkt.IsKeyFrame)
	}
}

func (r *Relay) relayAudioStream() {
	defer r.Stop()

	conn := r.session.AudioConn
	pktCount := 0
	for {
		pkt, raw, err := protocol.ReadPacket(conn)
		if err != nil {
			if err != io.EOF {
				log.Printf("[%s] audio stream error: %v", r.session.Serial, err)
			}
			log.Printf("[%s] audio stream ended after %d packets", r.session.Serial, pktCount)
			return
		}

		select {
		case <-r.stopCh:
			return
		default:
		}

		pktCount++
		if pktCount <= 3 || pktCount%1000 == 0 {
			log.Printf("[%s] audio pkt #%d: config=%v size=%d",
				r.session.Serial, pktCount, pkt.IsConfig, len(pkt.Data))
		}

		frame := make([]byte, 1+len(raw))
		frame[0] = ChannelAudio
		copy(frame[1:], raw)

		if pkt.IsConfig {
			r.mu.Lock()
			r.audioConfig = frame
			r.mu.Unlock()
		}

		r.broadcast(frame)
	}
}

func (r *Relay) relayDeviceMessages() {
	conn := r.session.ControlConn
	for {
		select {
		case <-r.stopCh:
			return
		default:
		}

		raw, err := protocol.ReadDeviceMsg(conn)
		if err != nil {
			if err != io.EOF {
				log.Printf("[%s] device msg error: %v", r.session.Serial, err)
			}
			return
		}

		frame := make([]byte, 1+len(raw))
		frame[0] = ChannelDevice
		copy(frame[1:], raw)

		r.broadcast(frame)
	}
}

func buildMgmtMsg(data map[string]interface{}) []byte {
	// Simple JSON serialization
	var sb []byte
	sb = append(sb, ChannelMgmt)

	// Manual JSON build to avoid import cycle
	sb = append(sb, '{')
	first := true
	for k, v := range data {
		if !first {
			sb = append(sb, ',')
		}
		first = false
		sb = append(sb, '"')
		sb = append(sb, k...)
		sb = append(sb, '"')
		sb = append(sb, ':')
		switch val := v.(type) {
		case string:
			sb = append(sb, '"')
			sb = append(sb, val...)
			sb = append(sb, '"')
		case int:
			sb = append(sb, []byte(intToStr(val))...)
		case bool:
			if val {
				sb = append(sb, "true"...)
			} else {
				sb = append(sb, "false"...)
			}
		}
	}
	sb = append(sb, '}')
	return sb
}

func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	if n < 0 {
		return "-" + intToStr(-n)
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
