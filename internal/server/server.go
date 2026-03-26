package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Fubuki233/scrhub/internal/device"
	"github.com/Fubuki233/scrhub/internal/relay"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

// Server is the HTTP + WebSocket server.
type Server struct {
	manager    *device.Manager
	listen     string
	listenTLS  string // empty = no TLS
	frontendFS fs.FS
	relays     map[string]*relay.Relay // serial → Relay
	relaysMu   sync.RWMutex
	clientID   uint64
	clientMu   sync.Mutex

	upgrader websocket.Upgrader
}

// New creates a new server.
func New(manager *device.Manager, listen, listenTLS string, frontendFS fs.FS) *Server {
	return &Server{
		manager:    manager,
		listen:     listen,
		listenTLS:  listenTLS,
		frontendFS: frontendFS,
		relays:     make(map[string]*relay.Relay),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true // LAN deployment, allow all origins
			},
			ReadBufferSize:  4096,
			WriteBufferSize: 64 * 1024,
		},
	}
}

// Run starts the HTTP server.
func (s *Server) Run(ctx context.Context) error {
	r := mux.NewRouter()

	// API routes
	api := r.PathPrefix("/api").Subrouter()
	api.HandleFunc("/defaults", s.handleDefaults).Methods("GET")
	api.HandleFunc("/devices", s.handleListDevices).Methods("GET")
	api.HandleFunc("/devices/{serial}/connect", s.handleConnect).Methods("POST")
	api.HandleFunc("/devices/{serial}/disconnect", s.handleDisconnect).Methods("POST")
	api.HandleFunc("/devices/{serial}/status", s.handleStatus).Methods("GET")
	api.HandleFunc("/devices/{serial}/tcpip", s.handleEnableTCPIP).Methods("POST")
	api.HandleFunc("/adb/connect", s.handleADBConnect).Methods("POST")
	api.HandleFunc("/adb/disconnect", s.handleADBDisconnect).Methods("POST")

	// WebSocket
	r.HandleFunc("/ws/device/{serial}", s.handleWebSocket)

	// Frontend static files (no-cache for development)
	staticHandler := http.FileServer(http.FS(s.frontendFS))
	r.PathPrefix("/").Handler(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		staticHandler.ServeHTTP(w, req)
	}))

	srv := &http.Server{
		Addr:    s.listen,
		Handler: r,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("web-scrcpy server listening on %s (HTTP)", s.listen)

	// Start HTTPS server if TLS address is configured
	if s.listenTLS != "" {
		tlsCert, err := generateSelfSignedCert()
		if err != nil {
			log.Printf("WARNING: failed to generate TLS cert: %v (HTTPS disabled)", err)
		} else {
			tlsSrv := &http.Server{
				Addr:    s.listenTLS,
				Handler: r,
				TLSConfig: &tls.Config{
					Certificates: []tls.Certificate{tlsCert},
				},
			}
			go func() {
				<-ctx.Done()
				shutdownCtx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel2()
				tlsSrv.Shutdown(shutdownCtx2)
			}()
			go func() {
				log.Printf("web-scrcpy server listening on %s (HTTPS - use this for LAN access)", s.listenTLS)
				if err := tlsSrv.ListenAndServeTLS("", ""); err != http.ErrServerClosed {
					log.Printf("HTTPS server error: %v", err)
				}
			}()
		}
	}

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) handleDefaults(w http.ResponseWriter, r *http.Request) {
	opts := s.manager.DefaultOpts()
	writeJSON(w, map[string]interface{}{
		"max_size":    opts.MaxSize,
		"bit_rate":    opts.BitRate,
		"max_fps":     opts.MaxFPS,
		"video_codec": opts.VideoCodec,
		"audio_codec": opts.AudioCodec,
	})
}

func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := s.manager.ListDevices(r.Context())
	if err != nil {
		log.Printf("ERROR: list devices: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if devices == nil {
		devices = make([]device.DeviceStatus, 0)
	}
	writeJSON(w, devices)
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	serial := mux.Vars(r)["serial"]

	// Parse optional JSON body for per-session options override
	var reqOpts struct {
		MaxSize    *uint16 `json:"max_size"`
		BitRate    *uint32 `json:"bit_rate"`
		MaxFPS     *string `json:"max_fps"`
		VideoCodec *string `json:"video_codec"`
		AudioCodec *string `json:"audio_codec"`
	}
	_ = json.NewDecoder(r.Body).Decode(&reqOpts) // ignore errors (body may be empty)

	var opts *device.SessionOptions
	defaults := s.manager.DefaultOpts()
	if reqOpts.MaxSize != nil || reqOpts.BitRate != nil || reqOpts.MaxFPS != nil || reqOpts.VideoCodec != nil || reqOpts.AudioCodec != nil {
		merged := defaults
		if reqOpts.MaxSize != nil {
			merged.MaxSize = *reqOpts.MaxSize
		}
		if reqOpts.BitRate != nil {
			merged.BitRate = *reqOpts.BitRate
		}
		if reqOpts.MaxFPS != nil {
			merged.MaxFPS = *reqOpts.MaxFPS
		}
		if reqOpts.VideoCodec != nil {
			merged.VideoCodec = *reqOpts.VideoCodec
		}
		if reqOpts.AudioCodec != nil {
			merged.AudioCodec = *reqOpts.AudioCodec
		}
		opts = &merged
	}

	sess, err := s.manager.Connect(r.Context(), serial, opts)
	if err != nil {
		log.Printf("ERROR: connect %s: %v", serial, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Create relay for this session
	s.relaysMu.Lock()
	if existing, ok := s.relays[serial]; ok {
		existing.Stop()
	}
	rl := relay.NewRelay(sess)
	s.relays[serial] = rl
	s.relaysMu.Unlock()

	rl.Start()

	writeJSON(w, map[string]interface{}{
		"status":      "connected",
		"device_name": sess.Info.DeviceName,
		"video_codec": device.DescribeCodec(sess.Info.VideoCodec),
		"audio_codec": device.DescribeCodec(sess.Info.AudioCodec),
		"width":       sess.Info.VideoWidth,
		"height":      sess.Info.VideoHeight,
	})
}

func (s *Server) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	serial := mux.Vars(r)["serial"]

	// Stop relay
	s.relaysMu.Lock()
	if rl, ok := s.relays[serial]; ok {
		rl.Stop()
		delete(s.relays, serial)
	}
	s.relaysMu.Unlock()

	if err := s.manager.Disconnect(serial); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	writeJSON(w, map[string]string{"status": "disconnected"})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	serial := mux.Vars(r)["serial"]
	sess := s.manager.GetSession(serial)
	if sess == nil {
		writeJSON(w, map[string]interface{}{
			"connected": false,
		})
		return
	}

	s.relaysMu.RLock()
	rl := s.relays[serial]
	viewers := 0
	if rl != nil {
		viewers = rl.ClientCount()
	}
	s.relaysMu.RUnlock()

	writeJSON(w, map[string]interface{}{
		"connected":   true,
		"device_name": sess.Info.DeviceName,
		"video_codec": device.DescribeCodec(sess.Info.VideoCodec),
		"audio_codec": device.DescribeCodec(sess.Info.AudioCodec),
		"width":       sess.Info.VideoWidth,
		"height":      sess.Info.VideoHeight,
		"viewers":     viewers,
		"settings": map[string]interface{}{
			"max_size":  sess.Options.MaxSize,
			"bit_rate":  sess.Options.BitRate,
			"max_fps":   sess.Options.MaxFPS,
		},
	})
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	serial := mux.Vars(r)["serial"]

	s.relaysMu.RLock()
	rl := s.relays[serial]
	s.relaysMu.RUnlock()

	if rl == nil {
		http.Error(w, "device not connected", http.StatusBadRequest)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket upgrade error: %v", err)
		return
	}

	clientID := s.nextClientID()
	client := relay.NewClient(clientID, conn)

	// Send device info as first management message
	sess := s.manager.GetSession(serial)
	if sess != nil {
		infoMsg := map[string]interface{}{
			"type":        "device_info",
			"device_name": sess.Info.DeviceName,
			"video_codec": device.DescribeCodec(sess.Info.VideoCodec),
			"audio_codec": device.DescribeCodec(sess.Info.AudioCodec),
			"width":       sess.Info.VideoWidth,
			"height":      sess.Info.VideoHeight,
		}
		data, _ := json.Marshal(infoMsg)
		frame := make([]byte, 1+len(data))
		frame[0] = relay.ChannelMgmt
		copy(frame[1:], data)
		client.Send(frame)
	}

	rl.AddClient(client)

	// Start write pump in goroutine
	go client.WritePump()

	// Read pump in this goroutine
	defer rl.RemoveClient(clientID)
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if len(msg) < 2 {
			continue
		}

		channel := msg[0]
		payload := msg[1:]

		switch channel {
		case relay.ChannelControl:
			rl.HandleControlMsg(clientID, payload)

		case relay.ChannelMgmt:
			s.handleMgmtMsg(rl, clientID, payload, conn)
		}
	}
}

func (s *Server) handleMgmtMsg(rl *relay.Relay, clientID string, payload []byte, conn *websocket.Conn) {
	var msg map[string]interface{}
	if err := json.Unmarshal(payload, &msg); err != nil {
		return
	}

	msgType, _ := msg["type"].(string)
	switch msgType {
	case "request_control":
		granted := rl.RequestControl(clientID)
		resp := map[string]interface{}{
			"type":    "control_granted",
			"granted": granted,
		}
		data, _ := json.Marshal(resp)
		frame := make([]byte, 1+len(data))
		frame[0] = relay.ChannelMgmt
		copy(frame[1:], data)
		conn.WriteMessage(websocket.BinaryMessage, frame)

	case "release_control":
		rl.ReleaseControl(clientID)
		resp := map[string]interface{}{
			"type":    "control_released",
			"success": true,
		}
		data, _ := json.Marshal(resp)
		frame := make([]byte, 1+len(data))
		frame[0] = relay.ChannelMgmt
		copy(frame[1:], data)
		conn.WriteMessage(websocket.BinaryMessage, frame)
	}
}

func (s *Server) nextClientID() string {
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	s.clientID++
	return fmt.Sprintf("client-%d", s.clientID)
}

// StopAllRelays stops all relay instances.
func (s *Server) StopAllRelays() {
	s.relaysMu.Lock()
	defer s.relaysMu.Unlock()
	for _, rl := range s.relays {
		rl.Stop()
	}
	s.relays = make(map[string]*relay.Relay)
}

func (s *Server) handleADBConnect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Address string `json:"address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Address == "" {
		http.Error(w, "address is required", http.StatusBadRequest)
		return
	}

	// Validate address format: must be ip:port
	if !isValidADBAddress(req.Address) {
		http.Error(w, "invalid address format, expected ip:port", http.StatusBadRequest)
		return
	}

	if err := s.manager.ADBConnect(r.Context(), req.Address); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"status": "connected", "address": req.Address})
}

func (s *Server) handleADBDisconnect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Address string `json:"address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Address == "" {
		http.Error(w, "address is required", http.StatusBadRequest)
		return
	}

	if err := s.manager.ADBDisconnect(r.Context(), req.Address); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"status": "disconnected"})
}

func (s *Server) handleEnableTCPIP(w http.ResponseWriter, r *http.Request) {
	serial := mux.Vars(r)["serial"]

	var req struct {
		Port int `json:"port"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Port == 0 {
		req.Port = 5555
	}

	addr, err := s.manager.EnableTCPIP(r.Context(), serial, req.Port)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"status": "ok", "address": addr})
}

// isValidADBAddress checks that the address is a valid ip:port.
func isValidADBAddress(addr string) bool {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil || host == "" || portStr == "" {
		return false
	}
	// Validate port is a number
	port := 0
	for _, c := range portStr {
		if c < '0' || c > '9' {
			return false
		}
		port = port*10 + int(c-'0')
	}
	if port < 1 || port > 65535 {
		return false
	}
	// Validate host is an IP (not a hostname to prevent SSRF)
	if net.ParseIP(host) == nil {
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
