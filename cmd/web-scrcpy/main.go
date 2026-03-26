package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	webscrcpy "github.com/Fubuki233/scrhub"
	"github.com/Fubuki233/scrhub/internal/adb"
	"github.com/Fubuki233/scrhub/internal/device"
	"github.com/Fubuki233/scrhub/internal/server"
)

// findScrcpyServer searches common installation paths for scrcpy-server.
func findScrcpyServer() string {
	paths := []string{
		"/usr/share/scrcpy/scrcpy-server",
		"/usr/local/share/scrcpy/scrcpy-server",
		"/snap/scrcpy/current/usr/share/scrcpy/scrcpy-server",
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func main() {
	listen := flag.String("listen", ":8080", "HTTP listen address")
	listenTLS := flag.String("https", ":8443", "HTTPS listen address (auto-generates self-signed cert; empty to disable)")
	serverPath := flag.String("server-path", "", "Path to scrcpy-server file (auto-detected if omitted)")
	adbPath := flag.String("adb", "adb", "Path to adb executable")
	maxSize := flag.Int("max-size", 1920, "Default video max resolution dimension (0=native)")
	videoCodec := flag.String("video-codec", "h264", "Default video codec (h264, h265, av1)")
	audioCodec := flag.String("audio-codec", "opus", "Default audio codec (opus, aac, flac, raw)")
	bitRate := flag.Int("bit-rate", 8000000, "Default video bitrate in bps")
	maxFPS := flag.String("max-fps", "60", "Default max FPS")
	flag.Parse()

	// Auto-detect scrcpy-server path if not specified
	if *serverPath == "" {
		*serverPath = findScrcpyServer()
		if *serverPath == "" {
			log.Fatal("scrcpy-server not found. Install scrcpy or specify --server-path")
		}
		log.Printf("auto-detected scrcpy-server: %s", *serverPath)
	}

	opts := device.SessionOptions{
		VideoCodec: *videoCodec,
		AudioCodec: *audioCodec,
		MaxSize:    uint16(*maxSize),
		BitRate:    uint32(*bitRate),
		MaxFPS:     *maxFPS,
	}

	a := adb.New(*adbPath)
	mgr := device.NewManager(a, *serverPath, opts)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Auto-discover devices
	mgr.StartAutoDiscovery(ctx, 5*time.Second)

	// Extract embedded frontend FS
	frontendSub, err := webscrcpy.FrontendFS()
	if err != nil {
		log.Fatalf("frontend fs: %v", err)
	}

	srv := server.New(mgr, *listen, *listenTLS, frontendSub)

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("shutting down...")
		srv.StopAllRelays()
		mgr.StopAll()
		cancel()
	}()

	if err := srv.Run(ctx); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
