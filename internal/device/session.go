package device

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Fubuki233/scrhub/internal/adb"
	"github.com/Fubuki233/scrhub/internal/protocol"
)

const (
	scrcpyVersion    = "3.3.4"
	deviceServerPath = "/data/local/tmp/scrcpy-server.jar"
	socketNamePrefix = "scrcpy_"
)

// SessionOptions controls how the scrcpy session is configured.
type SessionOptions struct {
	VideoCodec string // "h264", "h265", "av1" (default: "h264")
	AudioCodec string // "opus", "aac", "flac", "raw" (default: "opus")
	MaxSize    uint16 // max resolution dimension (0 = unlimited)
	BitRate    uint32 // video bitrate in bps (0 = default)
	MaxFPS     string // max fps as string (e.g., "30")
	AudioBitRate uint32
}

// DefaultOptions returns sensible defaults for web streaming.
func DefaultOptions() SessionOptions {
	return SessionOptions{
		VideoCodec: "h264",
		AudioCodec: "opus",
		MaxSize:    0,
		BitRate:    8_000_000,
		MaxFPS:     "60",
	}
}

// SessionInfo holds device/stream metadata after connection.
type SessionInfo struct {
	DeviceName string
	VideoCodec uint32
	AudioCodec uint32
	VideoWidth uint32
	VideoHeight uint32
}

// Session manages a single scrcpy connection to an Android device.
type Session struct {
	Serial  string
	Info    SessionInfo
	Options SessionOptions

	VideoConn   net.Conn
	AudioConn   net.Conn
	ControlConn net.Conn

	adb        *adb.ADB
	serverCmd  *exec.Cmd
	scid       uint32
	socketName string
	listener   net.Listener // for reverse mode
	forward    bool         // true = forward mode (TCP/IP), false = reverse mode (USB)
	localPort  int          // local port for adb forward

	mu         sync.Mutex
	running    bool
	connecting bool
	cancelFn   context.CancelFunc
}

// NewSession creates a session (not yet started).
func NewSession(a *adb.ADB, serial string, opts SessionOptions) *Session {
	return &Session{
		Serial:  serial,
		Options: opts,
		adb:     a,
	}
}

// Start pushes the server, opens tunnels, and establishes the 3 TCP streams.
func (s *Session) Start(ctx context.Context, serverPath string) error {
	s.mu.Lock()
	if s.running || s.connecting {
		s.mu.Unlock()
		return fmt.Errorf("session already running for %s", s.Serial)
	}
	s.connecting = true
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.connecting = false
		s.mu.Unlock()
	}()

	// Use a background context so the session survives the HTTP request
	// that initiated it. The HTTP request context must NOT propagate here,
	// since the session must keep running after the response is sent.
	sessionCtx, cancel := context.WithCancel(context.Background())
	s.cancelFn = cancel

	// 1. Push scrcpy-server.jar
	log.Printf("[%s] pushing scrcpy-server...", s.Serial)
	if err := s.adb.Push(sessionCtx, s.Serial, serverPath, deviceServerPath); err != nil {
		cancel()
		return fmt.Errorf("push server: %w", err)
	}

	// 2. Generate SCID
	s.scid = generateSCID()
	s.socketName = fmt.Sprintf("%s%08x", socketNamePrefix, s.scid)

	// 3. Determine tunnel mode: TCP/IP devices (serial contains ":") must use forward
	isTCPIP := strings.Contains(s.Serial, ":")
	if !isTCPIP {
		// USB device: try reverse tunnel first
		localPort, listener, err := s.setupReverseTunnel(sessionCtx)
		if err != nil {
			log.Printf("[%s] adb reverse failed (%v), falling back to forward mode", s.Serial, err)
			isTCPIP = true // fall back
		} else {
			s.listener = listener
			s.forward = false
			s.localPort = localPort
		}
	}
	if isTCPIP {
		// TCP/IP device or reverse failed: use forward tunnel
		localPort, err := s.setupForwardTunnel(sessionCtx)
		if err != nil {
			cancel()
			return fmt.Errorf("setup tunnel: %w", err)
		}
		s.forward = true
		s.localPort = localPort
	}

	// 4. Start scrcpy server on device
	log.Printf("[%s] starting scrcpy server (scid=%08x, port=%d, forward=%v)...", s.Serial, s.scid, s.localPort, s.forward)
	if err := s.startServer(sessionCtx); err != nil {
		s.cleanupTunnel(sessionCtx)
		cancel()
		return fmt.Errorf("start server: %w", err)
	}

	// 5. Establish 3 connections (video, audio, control)
	if s.forward {
		log.Printf("[%s] connecting to device (forward mode)...", s.Serial)
		if err := s.connectForward(sessionCtx); err != nil {
			s.Stop()
			return fmt.Errorf("forward connect: %w", err)
		}
	} else {
		log.Printf("[%s] waiting for device connections (reverse mode)...", s.Serial)
		if err := s.acceptConnections(sessionCtx); err != nil {
			s.Stop()
			return fmt.Errorf("accept connections: %w", err)
		}
	}

	// 6. Read device info from first socket
	if err := s.readDeviceInfo(); err != nil {
		s.Stop()
		return fmt.Errorf("read device info: %w", err)
	}

	// 7. Read codec IDs and video size
	if err := s.readStreamInfo(); err != nil {
		s.Stop()
		return fmt.Errorf("read stream info: %w", err)
	}

	s.mu.Lock()
	s.running = true
	s.mu.Unlock()

	log.Printf("[%s] session started: device=%q video=%s(%dx%d) audio=%s",
		s.Serial, s.Info.DeviceName,
		protocol.CodecName(s.Info.VideoCodec), s.Info.VideoWidth, s.Info.VideoHeight,
		protocol.CodecName(s.Info.AudioCodec))

	return nil
}

// Stop tears down the session.
func (s *Session) Stop() {
	s.mu.Lock()
	wasRunning := s.running
	s.running = false
	s.mu.Unlock()

	if s.cancelFn != nil {
		s.cancelFn()
	}

	if s.VideoConn != nil {
		s.VideoConn.Close()
	}
	if s.AudioConn != nil {
		s.AudioConn.Close()
	}
	if s.ControlConn != nil {
		s.ControlConn.Close()
	}
	if s.listener != nil {
		s.listener.Close()
	}
	if s.serverCmd != nil && s.serverCmd.Process != nil {
		s.serverCmd.Process.Kill()
	}

	// Cleanup tunnel
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.cleanupTunnel(ctx)

	if wasRunning {
		log.Printf("[%s] session stopped", s.Serial)
	}
}

// IsRunning returns whether the session is active.
// IsRunning returns whether the session is active or connecting.
func (s *Session) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running || s.connecting
}

func (s *Session) setupReverseTunnel(ctx context.Context) (int, net.Listener, error) {
	// Listen on a random port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, nil, fmt.Errorf("listen: %w", err)
	}

	port := listener.Addr().(*net.TCPAddr).Port
	// Map device abstract socket → host TCP port
	// scrcpy-server connects to localabstract:scrcpy_SCID in reverse mode
	remote := fmt.Sprintf("localabstract:%s", s.socketName)
	local := fmt.Sprintf("tcp:%d", port)

	err = s.adb.Reverse(ctx, s.Serial, remote, local)
	if err != nil {
		listener.Close()
		return 0, nil, fmt.Errorf("adb reverse: %w", err)
	}

	return port, listener, nil
}

// setupForwardTunnel uses adb forward for TCP/IP connected devices.
// Maps a random local TCP port to the device's abstract socket.
func (s *Session) setupForwardTunnel(ctx context.Context) (int, error) {
	// Find a free local port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("find free port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	local := fmt.Sprintf("tcp:%d", port)
	remote := fmt.Sprintf("localabstract:%s", s.socketName)

	err = s.adb.Forward(ctx, s.Serial, local, remote)
	if err != nil {
		return 0, fmt.Errorf("adb forward: %w", err)
	}

	return port, nil
}

func (s *Session) cleanupTunnel(ctx context.Context) {
	if s.forward {
		local := fmt.Sprintf("tcp:%d", s.localPort)
		_ = s.adb.ForwardRemove(ctx, s.Serial, local)
	} else if s.socketName != "" {
		remote := fmt.Sprintf("localabstract:%s", s.socketName)
		_ = s.adb.ReverseRemove(ctx, s.Serial, remote)
	}
}

func (s *Session) startServer(ctx context.Context) error {
	args := []string{
		fmt.Sprintf("CLASSPATH=%s", deviceServerPath),
		"app_process",
		"/",
		"com.genymobile.scrcpy.Server",
		scrcpyVersion,
		fmt.Sprintf("scid=%08x", s.scid),
		"log_level=info",
		fmt.Sprintf("tunnel_forward=%v", s.forward),
	}

	opts := s.Options

	if opts.VideoCodec != "" && opts.VideoCodec != "h264" {
		args = append(args, fmt.Sprintf("video_codec=%s", opts.VideoCodec))
	}
	if opts.AudioCodec != "" && opts.AudioCodec != "opus" {
		args = append(args, fmt.Sprintf("audio_codec=%s", opts.AudioCodec))
	}
	if opts.BitRate > 0 {
		args = append(args, fmt.Sprintf("video_bit_rate=%d", opts.BitRate))
	}
	if opts.AudioBitRate > 0 {
		args = append(args, fmt.Sprintf("audio_bit_rate=%d", opts.AudioBitRate))
	}
	if opts.MaxSize > 0 {
		args = append(args, fmt.Sprintf("max_size=%d", opts.MaxSize))
	}
	if opts.MaxFPS != "" {
		args = append(args, fmt.Sprintf("max_fps=%s", opts.MaxFPS))
	}

	cmd, err := s.adb.ShellStart(ctx, s.Serial, args...)
	if err != nil {
		return err
	}
	s.serverCmd = cmd
	return nil
}

func (s *Session) acceptConnections(ctx context.Context) error {
	// Reverse mode: device connects to us
	deadline := time.Now().Add(30 * time.Second)

	accept := func(name string) (net.Conn, error) {
		if dl, ok := s.listener.(interface{ SetDeadline(time.Time) error }); ok {
			dl.SetDeadline(deadline)
		}
		conn, err := s.listener.Accept()
		if err != nil {
			return nil, fmt.Errorf("accept %s: %w", name, err)
		}
		log.Printf("[%s] accepted %s connection", s.Serial, name)
		return conn, nil
	}

	var err error
	s.VideoConn, err = accept("video")
	if err != nil {
		return err
	}
	s.AudioConn, err = accept("audio")
	if err != nil {
		return err
	}
	s.ControlConn, err = accept("control")
	if err != nil {
		return err
	}

	// Disable Nagle's algorithm on control socket
	if tc, ok := s.ControlConn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}

	return nil
}

// connectForward connects to the device in forward mode.
// The server is listening on the device, we connect through the adb forward tunnel.
// Only the first accepted stream receives a dummy byte from the server.
func (s *Session) connectForward(ctx context.Context) error {
	addr := fmt.Sprintf("127.0.0.1:%d", s.localPort)
	maxAttempts := 100
	retryDelay := 100 * time.Millisecond

	dialOne := func(name string, firstConn bool) (net.Conn, error) {
		attempts := 1
		if firstConn {
			attempts = maxAttempts // first connection retries while server starts
		}
		for i := 0; i < attempts; i++ {
			conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err == nil {
				if firstConn {
					// Only the first stream gets a dummy byte from the server
					dummy := make([]byte, 1)
					conn.SetReadDeadline(time.Now().Add(5 * time.Second))
					_, readErr := conn.Read(dummy)
					conn.SetReadDeadline(time.Time{})
					if readErr != nil {
						conn.Close()
						if i < attempts-1 {
							time.Sleep(retryDelay)
							continue
						}
						return nil, fmt.Errorf("read dummy byte for %s: %w", name, readErr)
					}
				}
				log.Printf("[%s] connected %s (forward mode)", s.Serial, name)
				return conn, nil
			}
			if firstConn && i < attempts-1 {
				time.Sleep(retryDelay)
				continue
			}
			return nil, fmt.Errorf("dial %s: %w", name, err)
		}
		return nil, fmt.Errorf("dial %s: max attempts reached", name)
	}

	var err error
	s.VideoConn, err = dialOne("video", true)
	if err != nil {
		return err
	}
	s.AudioConn, err = dialOne("audio", false)
	if err != nil {
		return err
	}
	s.ControlConn, err = dialOne("control", false)
	if err != nil {
		return err
	}

	if tc, ok := s.ControlConn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}

	return nil
}

func (s *Session) readDeviceInfo() error {
	// Device name is read from the first socket (video)
	name, err := protocol.ReadDeviceName(s.VideoConn)
	if err != nil {
		return err
	}
	s.Info.DeviceName = name
	return nil
}

func (s *Session) readStreamInfo() error {
	// Read video codec ID
	codecID, err := protocol.ReadCodecID(s.VideoConn)
	if err != nil {
		return fmt.Errorf("video codec: %w", err)
	}
	if codecID == protocol.CodecDisabled || codecID == protocol.CodecError {
		return fmt.Errorf("video stream error or disabled: %d", codecID)
	}
	s.Info.VideoCodec = codecID

	// Read video size
	w, h, err := protocol.ReadVideoSize(s.VideoConn)
	if err != nil {
		return fmt.Errorf("video size: %w", err)
	}
	s.Info.VideoWidth = w
	s.Info.VideoHeight = h

	// Read audio codec ID
	audioCodecID, err := protocol.ReadCodecID(s.AudioConn)
	if err != nil {
		return fmt.Errorf("audio codec: %w", err)
	}
	s.Info.AudioCodec = audioCodecID

	return nil
}

func generateSCID() uint32 {
	var buf [4]byte
	rand.Read(buf[:])
	return binary.BigEndian.Uint32(buf[:]) & 0x7FFFFFFF // 31 bits
}

// SendControlMsg sends a raw control message to the device.
func (s *Session) SendControlMsg(data []byte) error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return fmt.Errorf("session not running")
	}
	s.mu.Unlock()

	if valid := protocol.ValidateControlMsg(data); valid <= 0 {
		return fmt.Errorf("invalid control message")
	}

	// Sanitize: reject unknown/dangerous message types
	msgType := data[0]
	if msgType > protocol.ControlMsgTypeResetVideo {
		return fmt.Errorf("unknown control message type: %d", msgType)
	}

	_, err := s.ControlConn.Write(data)
	return err
}

// DescribeCodec returns the WebCodecs codec string for the browser.
func DescribeCodec(codecID uint32) string {
	switch codecID {
	case protocol.CodecH264:
		return "avc1.640028" // H.264 High Profile Level 4.0
	case protocol.CodecH265:
		return "hev1.1.6.L93.B0"
	case protocol.CodecAV1:
		return "av01.0.04M.08"
	case protocol.CodecOpus:
		return "opus"
	case protocol.CodecAAC:
		return "mp4a.40.2"
	default:
		return ""
	}
}

// Sanitize validates that the serial only contains safe characters.
func sanitizeSerial(serial string) bool {
	for _, c := range serial {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == ':') {
			return false
		}
	}
	return len(serial) > 0 && len(serial) < 256
}

func init() {
	_ = strings.TrimSpace // avoid unused import
}
