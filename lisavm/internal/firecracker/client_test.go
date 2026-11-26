package firecracker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/softprobe/lisa/lisavm/internal/domain"
)

func TestWaitHealthyRequiresProvisionedDatabaseReady(t *testing.T) {
	var body string
	var status int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" || r.URL.Query().Get("require_db") != "1" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	targetAddr := strings.TrimPrefix(srv.URL, "http://")
	drv := NewDriver(Config{})
	drv.httpClient = &http.Client{
		Timeout: time.Second,
		Transport: &rewriteHostTransport{
			targetHostPort: targetAddr,
			base:           http.DefaultTransport,
		},
	}

	status = http.StatusOK
	body = `{"status":"ok"}`
	ctx1, cancel1 := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel1()
	if err := drv.WaitHealthy(ctx1, "172.16.0.2"); err == nil {
		t.Fatalf("expected WaitHealthy to reject plain HTTP 200 without database_ready=true")
	}

	status = http.StatusOK
	body = `{"ok":true,"database_configured":false,"database_ready":false}`
	ctx2, cancel2 := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel2()
	if err := drv.WaitHealthy(ctx2, "172.16.0.2"); err == nil {
		t.Fatalf("expected WaitHealthy to reject database_ready=false")
	}

	status = http.StatusOK
	body = `{"ok":true,"database_configured":true,"database_ready":true}`
	ctx3, cancel3 := context.WithTimeout(context.Background(), time.Second)
	defer cancel3()
	if err := drv.WaitHealthy(ctx3, "172.16.0.2"); err != nil {
		t.Fatalf("expected WaitHealthy to pass when database_ready=true, got: %v", err)
	}
}

func TestDriverUsesJailerStagesHardlinkedDiskAndIsolatesInstances(t *testing.T) {
	tmp := t.TempDir()
	kernel := filepath.Join(tmp, "vmlinux")
	if err := os.WriteFile(kernel, []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	fcBin := filepath.Join(tmp, "firecracker")
	if err := os.WriteFile(fcBin, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	captureDir := filepath.Join(tmp, "captures")
	if err := os.MkdirAll(captureDir, 0o700); err != nil {
		t.Fatal(err)
	}
	jailerScript := filepath.Join(tmp, "jailer")
	script := `#!/usr/bin/env python3
import json, os, socketserver, sys, http.server
from pathlib import Path
argv = sys.argv[1:]
inst_id = argv[argv.index("--id") + 1]
capture_dir = Path(` + jsonQuote(captureDir) + `)
(capture_dir / f"{inst_id}.json").write_text(json.dumps(argv))
chroot_base = Path(argv[argv.index("--chroot-base-dir") + 1])
exec_file = Path(argv[argv.index("--exec-file") + 1])
jail_root = chroot_base / exec_file.name / inst_id / "root"
jail_root.mkdir(parents=True, exist_ok=True)
fc_args = argv[argv.index("--") + 1:]
sock_path = jail_root / fc_args[fc_args.index("--api-sock") + 1].lstrip("/")
if sock_path.exists():
    sock_path.unlink()

class Handler(http.server.BaseHTTPRequestHandler):
    def do_PUT(self):
        n = int(self.headers.get("Content-Length", "0"))
        if n:
            self.rfile.read(n)
        if self.path == "/snapshot/create":
            (jail_root / "vmstate.snap").write_bytes(b"snap")
            (jail_root / "mem.bin").write_bytes(b"mem")
        self.send_response(204)
        self.end_headers()
    def do_PATCH(self):
        n = int(self.headers.get("Content-Length", "0"))
        if n:
            self.rfile.read(n)
        self.send_response(204)
        self.end_headers()
    def log_message(self, *_a):
        pass

class UnixHTTPServer(socketserver.UnixStreamServer):
    def server_bind(self):
        os.chdir(jail_root)
        self.socket.bind(sock_path.name)
        self.server_name = "localhost"
        self.server_port = 0

with UnixHTTPServer(str(sock_path), Handler) as srv:
    srv.serve_forever()
`
	if err := os.WriteFile(jailerScript, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	drv := NewDriver(Config{
		BinaryPath:    fcBin,
		JailerPath:    jailerScript,
		ChrootBaseDir: filepath.Join(tmp, "jailer-base"),
		KernelPath:    kernel,
		VCPUCount:     1,
		MemSizeMiB:    1024,
	})

	makeInst := func(id, ip, mac, tap string) *domain.Instance {
		instDir := filepath.Join(tmp, "instances", id)
		if err := os.MkdirAll(instDir, 0o700); err != nil {
			t.Fatal(err)
		}
		disk := filepath.Join(instDir, "disk.ext4")
		if err := os.WriteFile(disk, []byte("disk-"+id), 0o600); err != nil {
			t.Fatal(err)
		}
		return &domain.Instance{
			ID:         id,
			IP:         ip,
			GatewayIP:  "172.16.0.1",
			MAC:        mac,
			TAP:        tap,
			DiskPath:   disk,
			SocketPath: filepath.Join(instDir, "firecracker.sock"),
		}
	}

	instA := makeInst("lisa-aaa111", "172.16.0.2", "06:00:AC:10:00:02", "lvt-aaa111")
	instB := makeInst("lisa-bbb222", "172.16.0.3", "06:00:AC:10:00:03", "lvt-bbb222")

	pidA, err := drv.Boot(context.Background(), instA)
	if err != nil {
		t.Fatalf("Boot instA failed: %v", err)
	}
	instA.PID = pidA
	defer drv.Stop(context.Background(), instA)

	pidB, err := drv.Boot(context.Background(), instB)
	if err != nil {
		t.Fatalf("Boot instB failed: %v", err)
	}
	instB.PID = pidB
	defer drv.Stop(context.Background(), instB)

	hostDiskStat, err := os.Stat(instA.DiskPath)
	if err != nil {
		t.Fatal(err)
	}
	jailedDiskStat, err := os.Stat(filepath.Join(drv.jailRoot(instA.ID), "disk.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	hostIno := hostDiskStat.Sys().(*syscall.Stat_t).Ino
	jailedIno := jailedDiskStat.Sys().(*syscall.Stat_t).Ino
	if hostIno != jailedIno {
		t.Fatalf("expected jailed disk.ext4 to share inode with persistent disk (%d != %d)", hostIno, jailedIno)
	}

	var argsA, argsB []string
	if err := json.Unmarshal(mustRead(t, filepath.Join(captureDir, instA.ID+".json")), &argsA); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(mustRead(t, filepath.Join(captureDir, instB.ID+".json")), &argsB); err != nil {
		t.Fatal(err)
	}
	uidA := argVal(argsA, "--uid")
	uidB := argVal(argsB, "--uid")
	if uidA == "" || uidB == "" || uidA == uidB {
		t.Fatalf("expected distinct per-instance sandbox UIDs, got %q and %q", uidA, uidB)
	}

	snapPath := filepath.Join(tmp, "snapshots", instA.ID, "vmstate.snap")
	memPath := filepath.Join(tmp, "snapshots", instA.ID, "mem.bin")
	if err := drv.PauseAndSnapshot(context.Background(), instA, snapPath, memPath); err != nil {
		t.Fatalf("PauseAndSnapshot failed: %v", err)
	}
	if _, err := os.Stat(drv.jailInstanceDir(instA.ID)); !os.IsNotExist(err) {
		t.Fatalf("expected jail dir cleaned up after snapshot, got err=%v", err)
	}
	for _, p := range []string{snapPath, memPath} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("expected extracted snapshot file %s: %v", p, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("expected 0600 on %s, got %o", p, info.Mode().Perm())
		}
	}

	pidA2, err := drv.RestoreFromSnapshot(context.Background(), instA, snapPath, memPath)
	if err != nil {
		t.Fatalf("RestoreFromSnapshot failed: %v", err)
	}
	instA.PID = pidA2
	if err := drv.Stop(context.Background(), instA); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	if _, err := os.Stat(drv.jailInstanceDir(instA.ID)); !os.IsNotExist(err) {
		t.Fatalf("expected jail dir cleaned up after Stop, got err=%v", err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func argVal(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

type rewriteHostTransport struct {
	targetHostPort string
	base           http.RoundTripper
}

func (t *rewriteHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.URL.Host = net.JoinHostPort(strings.Split(t.targetHostPort, ":")[0], strings.Split(t.targetHostPort, ":")[1])
	return t.base.RoundTrip(cloned)
}
