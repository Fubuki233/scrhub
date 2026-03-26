package device

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Fubuki233/scrhub/internal/adb"
)

// Manager manages multiple device sessions.
type Manager struct {
	adb        *adb.ADB
	serverPath string
	options    SessionOptions

	mu       sync.RWMutex
	sessions map[string]*Session // serial → Session
}

// NewManager creates a device manager.
func NewManager(a *adb.ADB, serverPath string, opts SessionOptions) *Manager {
	m := &Manager{
		adb:        a,
		serverPath: serverPath,
		options:    opts,
		sessions:   make(map[string]*Session),
	}
	// Clean up stale scrcpy forward tunnels from previous runs
	m.cleanStaleForwards()
	return m
}

// DeviceStatus represents a device's connection status for the API.
type DeviceStatus struct {
	Serial     string       `json:"serial"`
	Model      string       `json:"model"`
	ADBState   string       `json:"adb_state"`   // "device", "offline", etc.
	ConnType   string       `json:"conn_type"`    // "usb" or "wifi"
	Connected  bool         `json:"connected"`    // has active scrcpy session
	Info       *SessionInfo `json:"info,omitempty"`
}

// ListDevices returns all ADB devices with their session status.
func (m *Manager) ListDevices(ctx context.Context) ([]DeviceStatus, error) {
	devices, err := m.adb.ListDevices(ctx)
	if err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]DeviceStatus, len(devices))
	for i, dev := range devices {
		connType := "usb"
		if strings.Contains(dev.Serial, ":") {
			connType = "wifi"
		}
		status := DeviceStatus{
			Serial:   dev.Serial,
			Model:    dev.Model,
			ADBState: dev.State,
			ConnType: connType,
		}
		if sess, ok := m.sessions[dev.Serial]; ok && sess.IsRunning() {
			status.Connected = true
			info := sess.Info
			status.Info = &info
		}
		result[i] = status
	}
	return result, nil
}

// DefaultOpts returns the server-wide default session options.
func (m *Manager) DefaultOpts() SessionOptions {
	return m.options
}

// Connect starts a scrcpy session for the given device.
// If opts is non-nil, it overrides the server defaults.
func (m *Manager) Connect(ctx context.Context, serial string, opts *SessionOptions) (*Session, error) {
	useOpts := m.options
	if opts != nil {
		useOpts = *opts
	}

	m.mu.Lock()
	if sess, ok := m.sessions[serial]; ok && sess.IsRunning() {
		m.mu.Unlock()
		return sess, nil // already connected
	}
	sess := NewSession(m.adb, serial, useOpts)
	m.sessions[serial] = sess
	m.mu.Unlock()

	if err := sess.Start(ctx, m.serverPath); err != nil {
		m.mu.Lock()
		delete(m.sessions, serial)
		m.mu.Unlock()
		return nil, err
	}

	return sess, nil
}

// Disconnect stops the scrcpy session for the given device.
func (m *Manager) Disconnect(serial string) error {
	m.mu.Lock()
	sess, ok := m.sessions[serial]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("no session for device %s", serial)
	}
	delete(m.sessions, serial)
	m.mu.Unlock()

	sess.Stop()
	return nil
}

// GetSession returns the session for a device, or nil if not connected.
func (m *Manager) GetSession(serial string) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sess := m.sessions[serial]
	if sess != nil && !sess.IsRunning() {
		return nil
	}
	return sess
}

// StopAll stops all active sessions.
func (m *Manager) StopAll() {
	m.mu.Lock()
	sessions := make(map[string]*Session)
	for k, v := range m.sessions {
		sessions[k] = v
	}
	m.sessions = make(map[string]*Session)
	m.mu.Unlock()

	for serial, sess := range sessions {
		log.Printf("stopping session for %s", serial)
		sess.Stop()
	}
}

// StartAutoDiscovery periodically checks for new/removed ADB devices.
func (m *Manager) StartAutoDiscovery(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.cleanupDeadSessions(ctx)
			}
		}
	}()
}

func (m *Manager) cleanupDeadSessions(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for serial, sess := range m.sessions {
		if !sess.IsRunning() {
			log.Printf("cleaning up dead session for %s", serial)
			delete(m.sessions, serial)
		}
	}
}

// ADBConnect connects to a device by IP:port over TCP/IP.
func (m *Manager) ADBConnect(ctx context.Context, addr string) error {
	return m.adb.ConnectTCPIP(ctx, addr)
}

// ADBDisconnect disconnects a TCP/IP device.
func (m *Manager) ADBDisconnect(ctx context.Context, addr string) error {
	return m.adb.DisconnectTCPIP(ctx, addr)
}

// EnableTCPIP switches a USB device to TCP/IP ADB mode and returns its IP.
func (m *Manager) EnableTCPIP(ctx context.Context, serial string, port int) (string, error) {
	// Get device IP before switching (need USB connection for shell)
	ip, err := m.adb.GetDeviceIP(ctx, serial)
	if err != nil {
		return "", fmt.Errorf("cannot detect device IP: %w", err)
	}

	// Switch to TCP/IP mode
	if err := m.adb.EnableTCPIP(ctx, serial, port); err != nil {
		return "", fmt.Errorf("enable tcpip: %w", err)
	}

	// Wait for device to restart adbd
	time.Sleep(2 * time.Second)

	// Connect over TCP/IP
	addr := fmt.Sprintf("%s:%d", ip, port)
	if err := m.adb.ConnectTCPIP(ctx, addr); err != nil {
		return "", fmt.Errorf("connect to %s: %w", addr, err)
	}

	return addr, nil
}

// cleanStaleForwards removes leftover scrcpy adb forward tunnels from previous runs.
func (m *Manager) cleanStaleForwards() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	devices, err := m.adb.ListDevices(ctx)
	if err != nil {
		return
	}

	for _, dev := range devices {
		lines, err := m.adb.ForwardList(ctx, dev.Serial)
		if err != nil {
			continue
		}
		for _, line := range lines {
			// Format: "SERIAL tcp:PORT localabstract:scrcpy_SCID"
			if !strings.Contains(line, "localabstract:scrcpy_") {
				continue
			}
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				local := parts[1] // e.g. "tcp:12345"
				if err := m.adb.ForwardRemove(ctx, dev.Serial, local); err != nil {
					log.Printf("failed to remove stale forward %s for %s: %v", local, dev.Serial, err)
				} else {
					log.Printf("removed stale forward %s for %s", local, dev.Serial)
				}
			}
		}
	}
}
