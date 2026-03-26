package adb

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"
)

// Device represents an ADB-connected Android device.
type Device struct {
	Serial      string
	State       string // "device", "offline", "unauthorized", etc.
	Model       string
	Product     string
	TransportID string
}

// ADB wraps the adb command-line tool.
type ADB struct {
	executable string
}

// New creates an ADB wrapper. If adbPath is empty, "adb" is used.
func New(adbPath string) *ADB {
	if adbPath == "" {
		adbPath = "adb"
	}
	return &ADB{executable: adbPath}
}

// ListDevices returns all connected devices.
// Returns an empty list (not an error) if adb is not available.
func (a *ADB) ListDevices(ctx context.Context) ([]Device, error) {
	out, err := a.run(ctx, "devices", "-l")
	if err != nil {
		// adb not installed or not reachable — return empty list
		log.Printf("WARNING: adb list devices failed: %v", err)
		return []Device{}, nil
	}

	var devices []Device
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "List of") {
			continue
		}
		dev := parseDeviceLine(line)
		if dev.Serial != "" {
			devices = append(devices, dev)
		}
	}
	return devices, nil
}

// Push pushes a local file to the device.
func (a *ADB) Push(ctx context.Context, serial, localPath, remotePath string) error {
	_, err := a.runWithSerial(ctx, serial, "push", localPath, remotePath)
	return err
}

// Shell runs a shell command on the device and returns the output.
func (a *ADB) Shell(ctx context.Context, serial string, args ...string) (string, error) {
	cmdArgs := append([]string{"shell"}, args...)
	return a.runWithSerial(ctx, serial, cmdArgs...)
}

// ShellStart starts a shell command on the device without waiting for it to finish.
// Returns the exec.Cmd so the caller can manage the process.
// Keeps stdin pipe open to prevent adb shell from closing the PTY (which would
// send SIGHUP and kill the remote process).
func (a *ADB) ShellStart(ctx context.Context, serial string, args ...string) (*exec.Cmd, error) {
	cmdArgs := []string{"-s", serial, "shell"}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.CommandContext(ctx, a.executable, cmdArgs...)

	// Keep stdin open so the adb shell PTY stays alive
	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("adb shell stdin pipe: %w", err)
	}
	_ = stdinPipe // held open; closed when cmd is killed

	// Capture stderr for adb diagnostic messages
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("adb shell stderr pipe: %w", err)
	}

	// Capture stdout for remote process output (scrcpy-server logs)
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("adb shell stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("adb shell start: %w", err)
	}

	// Log server output in background
	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		for scanner.Scan() {
			log.Printf("[scrcpy-%s] %s", serial, scanner.Text())
		}
	}()
	go func() {
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			log.Printf("[adb-%s] %s", serial, scanner.Text())
		}
	}()

	return cmd, nil
}

// Reverse sets up a reverse tunnel: device connects back to host.
func (a *ADB) Reverse(ctx context.Context, serial, remote, local string) error {
	_, err := a.runWithSerial(ctx, serial, "reverse", remote, local)
	return err
}

// ReverseRemove removes a reverse tunnel.
func (a *ADB) ReverseRemove(ctx context.Context, serial, remote string) error {
	_, err := a.runWithSerial(ctx, serial, "reverse", "--remove", remote)
	return err
}

// Forward sets up a forward tunnel.
func (a *ADB) Forward(ctx context.Context, serial, local, remote string) error {
	_, err := a.runWithSerial(ctx, serial, "forward", local, remote)
	return err
}

// ForwardRemove removes a forward tunnel.
func (a *ADB) ForwardRemove(ctx context.Context, serial, local string) error {
	_, err := a.runWithSerial(ctx, serial, "forward", "--remove", local)
	return err
}

// ForwardList lists all active forward tunnels for a device.
// Returns lines like "tcp:12345 localabstract:scrcpy_abcd1234".
func (a *ADB) ForwardList(ctx context.Context, serial string) ([]string, error) {
	out, err := a.runWithSerial(ctx, serial, "forward", "--list")
	if err != nil {
		return nil, err
	}
	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

// ConnectTCPIP connects to a device over TCP/IP.
func (a *ADB) ConnectTCPIP(ctx context.Context, addr string) error {
	out, err := a.run(ctx, "connect", addr)
	if err != nil {
		return err
	}
	// adb connect may return 0 even on failure, check output
	if strings.Contains(out, "cannot connect") || strings.Contains(out, "failed") {
		return fmt.Errorf("adb connect: %s", strings.TrimSpace(out))
	}
	return nil
}

// DisconnectTCPIP disconnects a TCP/IP device.
func (a *ADB) DisconnectTCPIP(ctx context.Context, addr string) error {
	_, err := a.run(ctx, "disconnect", addr)
	return err
}

// EnableTCPIP switches a USB device to TCP/IP mode on the given port.
func (a *ADB) EnableTCPIP(ctx context.Context, serial string, port int) error {
	out, err := a.runWithSerial(ctx, serial, "tcpip", fmt.Sprintf("%d", port))
	if err != nil {
		return err
	}
	if strings.Contains(out, "error") {
		return fmt.Errorf("adb tcpip: %s", strings.TrimSpace(out))
	}
	return nil
}

// GetDeviceIP tries to get the device's WiFi/hotspot IP address.
func (a *ADB) GetDeviceIP(ctx context.Context, serial string) (string, error) {
	// Try wlan0 first (normal WiFi), then ap0/swlan0 (hotspot interfaces)
	interfaces := []string{"wlan0", "ap0", "swlan0", "wlan1", "rndis0"}
	for _, iface := range interfaces {
		out, err := a.Shell(ctx, serial, "ip", "-f", "inet", "addr", "show", iface)
		if err != nil || out == "" {
			continue
		}
		// Parse "inet 192.168.x.x/24" from output
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "inet ") {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					ip := strings.Split(parts[1], "/")[0]
					if ip != "" && ip != "127.0.0.1" {
						return ip, nil
					}
				}
			}
		}
	}
	return "", fmt.Errorf("no IP address found on device %s", serial)
}

func (a *ADB) run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.executable, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("adb %s: %w: %s", strings.Join(args, " "), err, string(out))
	}
	return string(out), nil
}

func (a *ADB) runWithSerial(ctx context.Context, serial string, args ...string) (string, error) {
	fullArgs := append([]string{"-s", serial}, args...)
	return a.run(ctx, fullArgs...)
}

func parseDeviceLine(line string) Device {
	// Format: "SERIAL\tSTATE key:value key:value ..."
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return Device{}
	}
	dev := Device{
		Serial: parts[0],
		State:  parts[1],
	}
	for _, kv := range parts[2:] {
		idx := strings.Index(kv, ":")
		if idx < 0 {
			continue
		}
		key, val := kv[:idx], kv[idx+1:]
		switch key {
		case "model":
			dev.Model = val
		case "product":
			dev.Product = val
		case "transport_id":
			dev.TransportID = val
		}
	}
	return dev
}
