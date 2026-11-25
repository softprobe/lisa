package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/softprobe/lisa/lisavm/internal/domain"
)

type VMDriver interface {
	Boot(ctx context.Context, inst *domain.Instance) (int, error)
	PauseAndSnapshot(ctx context.Context, inst *domain.Instance, snapshotPath, memFilePath string) error
	RestoreFromSnapshot(ctx context.Context, inst *domain.Instance, snapshotPath, memFilePath string) (int, error)
	Stop(ctx context.Context, inst *domain.Instance) error
	WaitHealthy(ctx context.Context, ip string) error
	IsGuestBusy(ctx context.Context, ip string) (bool, error)
}

type Config struct {
	BinaryPath string
	KernelPath string
	VCPUCount  int
	MemSizeMiB int
}

type procHandle struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  chan struct{}
}

type Driver struct {
	cfg        Config
	httpClient *http.Client

	mu    sync.Mutex
	procs map[int]*procHandle
}

func NewDriver(cfg Config) *Driver {
	if cfg.BinaryPath == "" {
		cfg.BinaryPath = "firecracker"
	}
	if cfg.VCPUCount <= 0 {
		cfg.VCPUCount = 1
	}
	if cfg.MemSizeMiB <= 0 {
		cfg.MemSizeMiB = 512
	}
	return &Driver{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: 3 * time.Second,
		},
		procs: make(map[int]*procHandle),
	}
}

func (d *Driver) Boot(ctx context.Context, inst *domain.Instance) (int, error) {
	pid, err := d.startProcess(ctx, inst)
	if err != nil {
		return 0, err
	}

	sock := inst.SocketPath
	cleanupOnErr := func(cause error) (int, error) {
		_ = d.killPID(pid)
		_ = os.Remove(sock)
		return 0, cause
	}

	if err := d.apiCall(ctx, sock, http.MethodPut, "/machine-config", map[string]any{
		"vcpu_count":   d.cfg.VCPUCount,
		"mem_size_mib": d.cfg.MemSizeMiB,
	}); err != nil {
		return cleanupOnErr(fmt.Errorf("machine-config: %w", err))
	}

	bootArgs := fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 pci=off ip=%s::%s:255.255.0.0:%s:eth0:off",
		inst.IP, inst.GatewayIP, inst.ID,
	)
	if err := d.apiCall(ctx, sock, http.MethodPut, "/boot-source", map[string]any{
		"kernel_image_path": d.cfg.KernelPath,
		"boot_args":         bootArgs,
	}); err != nil {
		return cleanupOnErr(fmt.Errorf("boot-source: %w", err))
	}

	if err := d.apiCall(ctx, sock, http.MethodPut, "/drives/rootfs", map[string]any{
		"drive_id":       "rootfs",
		"path_on_host":   inst.DiskPath,
		"is_root_device": true,
		"is_read_only":   false,
	}); err != nil {
		return cleanupOnErr(fmt.Errorf("drives/rootfs: %w", err))
	}

	if err := d.apiCall(ctx, sock, http.MethodPut, "/network-interfaces/eth0", map[string]any{
		"iface_id":      "eth0",
		"guest_mac":     inst.MAC,
		"host_dev_name": inst.TAP,
	}); err != nil {
		return cleanupOnErr(fmt.Errorf("network-interfaces/eth0: %w", err))
	}

	if err := d.apiCall(ctx, sock, http.MethodPut, "/actions", map[string]any{
		"action_type": "InstanceStart",
	}); err != nil {
		return cleanupOnErr(fmt.Errorf("InstanceStart: %w", err))
	}

	return pid, nil
}

func (d *Driver) PauseAndSnapshot(ctx context.Context, inst *domain.Instance, snapshotPath, memFilePath string) error {
	if err := os.MkdirAll(filepath.Dir(snapshotPath), 0o755); err != nil {
		return fmt.Errorf("create snapshot dir: %w", err)
	}
	_ = os.Remove(snapshotPath)
	_ = os.Remove(memFilePath)

	if err := d.apiCall(ctx, inst.SocketPath, http.MethodPatch, "/vm", map[string]any{
		"state": "Paused",
	}); err != nil {
		return fmt.Errorf("pause vm: %w", err)
	}

	if err := d.apiCall(ctx, inst.SocketPath, http.MethodPut, "/snapshot/create", map[string]any{
		"snapshot_type": "Full",
		"snapshot_path": snapshotPath,
		"mem_file_path": memFilePath,
	}); err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}

	_ = d.killPID(inst.PID)
	_ = os.Remove(inst.SocketPath)
	return nil
}

func (d *Driver) RestoreFromSnapshot(ctx context.Context, inst *domain.Instance, snapshotPath, memFilePath string) (int, error) {
	pid, err := d.startProcess(ctx, inst)
	if err != nil {
		return 0, err
	}

	if err := d.apiCall(ctx, inst.SocketPath, http.MethodPut, "/snapshot/load", map[string]any{
		"snapshot_path": snapshotPath,
		"mem_file_path": memFilePath,
		"resume_vm":     true,
	}); err != nil {
		_ = d.killPID(pid)
		_ = os.Remove(inst.SocketPath)
		return 0, fmt.Errorf("load snapshot: %w", err)
	}
	return pid, nil
}

func (d *Driver) Stop(_ context.Context, inst *domain.Instance) error {
	if inst.PID > 0 {
		_ = d.killPID(inst.PID)
	}
	if inst.SocketPath != "" {
		_ = os.Remove(inst.SocketPath)
	}
	return nil
}

func (d *Driver) WaitHealthy(ctx context.Context, ip string) error {
	url := fmt.Sprintf("http://%s:8000/health", ip)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err == nil {
			resp, err := d.httpClient.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s", url)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
}

func (d *Driver) IsGuestBusy(ctx context.Context, ip string) (bool, error) {
	url := fmt.Sprintf("http://%s:8000/internal/v1/runtime/busy", ip)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("busy check returned status %d", resp.StatusCode)
	}
	var payload struct {
		Busy bool `json:"busy"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return false, fmt.Errorf("decode busy response: %w", err)
	}
	return payload.Busy, nil
}

func (d *Driver) startProcess(ctx context.Context, inst *domain.Instance) (int, error) {
	_ = os.Remove(inst.SocketPath)
	if err := os.MkdirAll(filepath.Dir(inst.SocketPath), 0o755); err != nil {
		return 0, fmt.Errorf("create socket dir: %w", err)
	}
	logPath := filepath.Join(filepath.Dir(inst.DiskPath), "firecracker.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, fmt.Errorf("open firecracker log: %w", err)
	}

	cmd := exec.Command(d.cfg.BinaryPath, "--api-sock", inst.SocketPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = logFile.Close()
		return 0, fmt.Errorf("stdin pipe: %w", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = logFile.Close()
		return 0, fmt.Errorf("start firecracker: %w", err)
	}

	pid := cmd.Process.Pid
	handle := &procHandle{
		cmd:   cmd,
		stdin: stdin,
		done:  make(chan struct{}),
	}
	d.mu.Lock()
	d.procs[pid] = handle
	d.mu.Unlock()

	go func() {
		_ = cmd.Wait()
		_ = stdin.Close()
		_ = logFile.Close()
		close(handle.done)
		d.mu.Lock()
		delete(d.procs, pid)
		d.mu.Unlock()
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if info, err := os.Stat(inst.SocketPath); err == nil && info.Mode()&os.ModeSocket != 0 {
			return pid, nil
		}
		select {
		case <-handle.done:
			return 0, fmt.Errorf("firecracker exited before socket appeared (see %s)", logPath)
		case <-ctx.Done():
			_ = d.killPID(pid)
			return 0, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			_ = d.killPID(pid)
			return 0, fmt.Errorf("timed out waiting for firecracker socket %s", inst.SocketPath)
		}
	}
}

func (d *Driver) killPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	d.mu.Lock()
	handle := d.procs[pid]
	d.mu.Unlock()

	if handle != nil {
		_ = handle.cmd.Process.Signal(syscall.SIGKILL)
		select {
		case <-handle.done:
		case <-time.After(3 * time.Second):
		}
		return nil
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	_ = proc.Signal(syscall.SIGKILL)
	return nil
}

func (d *Driver) apiCall(ctx context.Context, sockPath, method, path string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
			},
		},
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s %s -> %d: %s", method, path, resp.StatusCode, string(bytes.TrimSpace(msg)))
	}
	return nil
}
