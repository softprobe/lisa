package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	BinaryPath    string
	JailerPath    string
	ChrootBaseDir string
	KernelPath    string
	VCPUCount     int
	MemSizeMiB    int
	JailerUID     int
	JailerGID     int
	NetNS         string
}

type procHandle struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	log   *os.File
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
	if cfg.JailerPath == "" {
		cfg.JailerPath = "jailer"
	}
	if cfg.ChrootBaseDir == "" {
		cfg.ChrootBaseDir = "/var/lib/lisavm/jailer"
	}
	if cfg.VCPUCount <= 0 {
		cfg.VCPUCount = 1
	}
	if cfg.MemSizeMiB <= 0 {
		cfg.MemSizeMiB = 1024
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
	pid, err := d.startProcess(ctx, inst, false, "", "")
	if err != nil {
		return 0, err
	}

	sock := d.hostSocketPath(inst)
	cleanupOnErr := func(cause error) (int, error) {
		_ = d.killPID(pid)
		_ = os.Remove(sock)
		d.cleanupJail(inst)
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
		"kernel_image_path": "/vmlinux",
		"boot_args":         bootArgs,
	}); err != nil {
		return cleanupOnErr(fmt.Errorf("boot-source: %w", err))
	}

	if err := d.apiCall(ctx, sock, http.MethodPut, "/drives/rootfs", map[string]any{
		"drive_id":       "rootfs",
		"path_on_host":   "/disk.ext4",
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
	if err := os.MkdirAll(filepath.Dir(snapshotPath), 0o700); err != nil {
		return fmt.Errorf("create snapshot dir: %w", err)
	}
	_ = os.Chmod(filepath.Dir(snapshotPath), 0o700)
	_ = os.Remove(snapshotPath)
	_ = os.Remove(memFilePath)

	sock := d.hostSocketPath(inst)
	if err := d.apiCall(ctx, sock, http.MethodPatch, "/vm", map[string]any{
		"state": "Paused",
	}); err != nil {
		return fmt.Errorf("pause vm: %w", err)
	}

	if err := d.apiCall(ctx, sock, http.MethodPut, "/snapshot/create", map[string]any{
		"snapshot_type": "Full",
		"snapshot_path": "/vmstate.snap",
		"mem_file_path": "/mem.bin",
	}); err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}

	_ = d.killPID(inst.PID)
	_ = os.Remove(sock)

	jailRoot := d.jailRoot(inst.ID)
	if err := movePrivateFile(filepath.Join(jailRoot, "vmstate.snap"), snapshotPath); err != nil {
		return fmt.Errorf("store vmstate snapshot: %w", err)
	}
	if err := movePrivateFile(filepath.Join(jailRoot, "mem.bin"), memFilePath); err != nil {
		_ = os.Remove(snapshotPath)
		return fmt.Errorf("store mem snapshot: %w", err)
	}
	d.cleanupJail(inst)
	return nil
}

func (d *Driver) RestoreFromSnapshot(ctx context.Context, inst *domain.Instance, snapshotPath, memFilePath string) (int, error) {
	pid, err := d.startProcess(ctx, inst, true, snapshotPath, memFilePath)
	if err != nil {
		return 0, err
	}

	sock := d.hostSocketPath(inst)
	if err := d.apiCall(ctx, sock, http.MethodPut, "/snapshot/load", map[string]any{
		"snapshot_path": "/vmstate.snap",
		"mem_file_path": "/mem.bin",
		"resume_vm":     true,
	}); err != nil {
		_ = d.killPID(pid)
		_ = os.Remove(sock)
		d.cleanupJail(inst)
		return 0, fmt.Errorf("load snapshot: %w", err)
	}
	return pid, nil
}

func (d *Driver) Stop(_ context.Context, inst *domain.Instance) error {
	if inst.PID > 0 {
		_ = d.killPID(inst.PID)
	}
	_ = os.Remove(d.hostSocketPath(inst))
	if inst.SocketPath != "" {
		_ = os.Remove(inst.SocketPath)
	}
	d.cleanupJail(inst)
	return nil
}

func (d *Driver) WaitHealthy(ctx context.Context, ip string) error {
	url := fmt.Sprintf("http://%s:8000/health?require_db=1", ip)
	deadline := time.Now().Add(60 * time.Second)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err == nil {
			resp, err := d.httpClient.Do(req)
			if err == nil {
				var payload struct {
					OK                 bool `json:"ok"`
					DatabaseConfigured bool `json:"database_configured"`
					DatabaseReady      bool `json:"database_ready"`
				}
				decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&payload)
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK &&
					decodeErr == nil &&
					payload.OK &&
					payload.DatabaseConfigured &&
					payload.DatabaseReady {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for database-ready %s", url)
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
		return false, fmt.Errorf("busy check status %d", resp.StatusCode)
	}
	var payload struct {
		Busy bool `json:"busy"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return false, err
	}
	return payload.Busy, nil
}

func (d *Driver) startProcess(ctx context.Context, inst *domain.Instance, includeSnapshot bool, snapPath, memPath string) (int, error) {
	fcExec, err := exec.LookPath(d.cfg.BinaryPath)
	if err != nil {
		return 0, fmt.Errorf("locate firecracker binary %q: %w", d.cfg.BinaryPath, err)
	}
	jailerExec, err := exec.LookPath(d.cfg.JailerPath)
	if err != nil {
		return 0, fmt.Errorf("locate jailer binary %q: %w", d.cfg.JailerPath, err)
	}

	uid, gid := domain.SandboxUIDGID(inst.ID, inst.IP, d.cfg.JailerUID, d.cfg.JailerGID)
	if err := d.stageJail(inst, uid, gid, includeSnapshot, snapPath, memPath); err != nil {
		return 0, err
	}

	sockPath := d.hostSocketPath(inst)
	logPath := filepath.Join(filepath.Dir(inst.DiskPath), "firecracker.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		d.cleanupJail(inst)
		return 0, fmt.Errorf("open firecracker log: %w", err)
	}
	_ = os.Chmod(logPath, 0o600)

	args := []string{
		"--id", jailID(inst.ID),
		"--exec-file", fcExec,
		"--uid", strconv.Itoa(uid),
		"--gid", strconv.Itoa(gid),
		"--chroot-base-dir", d.cfg.ChrootBaseDir,
		"--cgroup-version", "2",
	}
	if d.cfg.NetNS != "" {
		args = append(args, "--netns", d.cfg.NetNS)
	}
	args = append(args, "--", "--api-sock", "/firecracker.sock")

	cmd := exec.Command(jailerExec, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = logFile.Close()
		d.cleanupJail(inst)
		return 0, fmt.Errorf("stdin pipe: %w", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = logFile.Close()
		d.cleanupJail(inst)
		return 0, fmt.Errorf("start jailer: %w", err)
	}

	pid := cmd.Process.Pid
	handle := &procHandle{
		cmd:   cmd,
		stdin: stdin,
		log:   logFile,
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
		if info, err := os.Stat(sockPath); err == nil && info.Mode()&os.ModeSocket != 0 {
			return pid, nil
		}
		select {
		case <-handle.done:
			d.cleanupJail(inst)
			return 0, fmt.Errorf("jailed firecracker exited before socket appeared (see %s)", logPath)
		case <-ctx.Done():
			_ = d.killPID(pid)
			d.cleanupJail(inst)
			return 0, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			_ = d.killPID(pid)
			d.cleanupJail(inst)
			return 0, fmt.Errorf("timed out waiting for jailed firecracker socket %s", sockPath)
		}
	}
}

func (d *Driver) stageJail(inst *domain.Instance, uid, gid int, includeSnapshot bool, snapPath, memPath string) error {
	jailInstDir := d.jailInstanceDir(inst.ID)
	jailRoot := d.jailRoot(inst.ID)
	if err := os.RemoveAll(jailInstDir); err != nil {
		return fmt.Errorf("clean stale jail dir: %w", err)
	}
	if err := os.MkdirAll(d.cfg.ChrootBaseDir, 0o700); err != nil {
		return fmt.Errorf("create jailer chroot base dir: %w", err)
	}
	_ = os.Chmod(d.cfg.ChrootBaseDir, 0o700)
	execJailParent := filepath.Dir(jailInstDir)
	if err := os.MkdirAll(execJailParent, 0o700); err != nil {
		return fmt.Errorf("create jailer exec parent dir: %w", err)
	}
	_ = os.Chmod(execJailParent, 0o700)
	if err := os.MkdirAll(jailInstDir, 0o700); err != nil {
		return fmt.Errorf("create jail instance dir: %w", err)
	}
	_ = os.Chmod(jailInstDir, 0o700)
	if err := os.MkdirAll(jailRoot, 0o700); err != nil {
		return fmt.Errorf("create jail root dir: %w", err)
	}
	_ = os.Chmod(jailRoot, 0o700)
	if os.Geteuid() == 0 {
		if err := os.Chown(jailRoot, uid, gid); err != nil {
			return fmt.Errorf("chown jail root dir: %w", err)
		}
	}

	// Hard-link disk.ext4 so guest writes land directly on the persistent instance inode; fail fast on cross-FS setup.
	if err := stageHardlinkedDisk(inst.DiskPath, filepath.Join(jailRoot, "disk.ext4"), uid, gid, 0o600); err != nil {
		return fmt.Errorf("stage disk.ext4: %w", err)
	}
	if d.cfg.KernelPath != "" {
		if err := linkOrCopyFile(d.cfg.KernelPath, filepath.Join(jailRoot, "vmlinux"), 0o444); err != nil {
			return fmt.Errorf("stage vmlinux: %w", err)
		}
	}
	if includeSnapshot {
		if err := stageOwnedFile(snapPath, filepath.Join(jailRoot, "vmstate.snap"), uid, gid, 0o600); err != nil {
			return fmt.Errorf("stage vmstate.snap: %w", err)
		}
		if err := stageOwnedFile(memPath, filepath.Join(jailRoot, "mem.bin"), uid, gid, 0o600); err != nil {
			return fmt.Errorf("stage mem.bin: %w", err)
		}
	}
	return nil
}

func (d *Driver) cleanupJail(inst *domain.Instance) {
	if inst.DiskPath != "" {
		reclaimHostFile(inst.DiskPath)
	}
	if inst.SnapshotPath != "" {
		reclaimHostFile(inst.SnapshotPath)
	}
	if inst.MemFilePath != "" {
		reclaimHostFile(inst.MemFilePath)
	}
	_ = os.RemoveAll(d.jailInstanceDir(inst.ID))
	_ = os.Remove(d.cgroupDir(inst.ID))
}

func (d *Driver) jailInstanceDir(id string) string {
	return filepath.Join(d.cfg.ChrootBaseDir, filepath.Base(d.cfg.BinaryPath), jailID(id))
}

func (d *Driver) jailRoot(id string) string {
	return filepath.Join(d.jailInstanceDir(id), "root")
}

func (d *Driver) cgroupDir(id string) string {
	return filepath.Join("/sys/fs/cgroup", filepath.Base(d.cfg.BinaryPath), jailID(id))
}

func (d *Driver) hostSocketPath(inst *domain.Instance) string {
	return filepath.Join(d.jailRoot(inst.ID), "firecracker.sock")
}

func jailID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
		if b.Len() == 64 {
			break
		}
	}
	if b.Len() == 0 {
		return "lisa-vm"
	}
	return b.String()
}

func stageHardlinkedDisk(src, dst string, uid, gid int, mode os.FileMode) error {
	if err := os.Chmod(src, mode); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(src, uid, gid); err != nil {
			return err
		}
	}
	if err := os.Link(src, dst); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return fmt.Errorf("jailer chroot base must reside on the same filesystem as %s so disk.ext4 can be hard-linked", src)
		}
		return err
	}
	return nil
}

func stageOwnedFile(src, dst string, uid, gid int, mode os.FileMode) error {
	if err := os.Chmod(src, mode); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(src, uid, gid); err != nil {
			return err
		}
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := copyFile(src, dst, mode); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		return os.Chown(dst, uid, gid)
	}
	return nil
}

func reclaimHostFile(path string) {
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		_ = os.Chmod(path, 0o600)
		if os.Geteuid() == 0 {
			_ = os.Chown(path, 0, 0)
		}
	}
}

func linkOrCopyFile(src, dst string, mode os.FileMode) error {
	if err := copyFile(src, dst, mode); err == nil {
		return nil
	}
	return os.Link(src, dst)
}

func movePrivateFile(src, dst string) error {
	if err := os.Chmod(src, 0o600); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(src, 0, 0); err != nil {
			return err
		}
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	if err := copyFile(src, dst, 0o600); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		if err := os.Chown(dst, 0, 0); err != nil {
			return err
		}
	}
	return os.Remove(src)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return os.Chmod(dst, mode)
}

func (d *Driver) killPID(pid int) error {
	if pid <= 0 {
		return nil
	}
	d.mu.Lock()
	handle := d.procs[pid]
	d.mu.Unlock()

	if handle != nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = handle.cmd.Process.Signal(syscall.SIGKILL)
		select {
		case <-handle.done:
		case <-time.After(3 * time.Second):
		}
		return nil
	}

	_ = syscall.Kill(-pid, syscall.SIGKILL)
	proc, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	_ = proc.Signal(syscall.SIGKILL)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return nil
}

func (d *Driver) apiCall(ctx context.Context, sockPath, method, path string, body any) error {
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}

	transport := &http.Transport{
		DialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			if len(sockPath) > 100 {
				dirFD, err := syscall.Open(filepath.Dir(sockPath), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
				if err == nil {
					defer syscall.Close(dirFD)
					shortSock := fmt.Sprintf("/proc/self/fd/%d/%s", dirFD, filepath.Base(sockPath))
					return dialer.DialContext(dialCtx, "unix", shortSock)
				}
			}
			return dialer.DialContext(dialCtx, "unix", sockPath)
		},
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	respBody, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("firecracker %s %s status %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(respBody))
}
