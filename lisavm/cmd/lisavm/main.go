package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/softprobe/lisa/lisavm/internal/firecracker"
	"github.com/softprobe/lisa/lisavm/internal/host"
	"github.com/softprobe/lisa/lisavm/internal/manager"
	"github.com/softprobe/lisa/lisavm/internal/server"
	"github.com/softprobe/lisa/lisavm/internal/store"
)

func main() {
	root := envOr("LISAVM_ROOT", "/var/lib/lisavm")
	listenAddr := envOr("LISAVM_LISTEN_ADDR", "127.0.0.1:7700")
	bridge := envOr("LISAVM_BRIDGE", "lisabr0")
	gatewayIP := envOr("LISAVM_GATEWAY_IP", "172.16.0.1")
	rootfsPath := envOr("LISAVM_ROOTFS_PATH", filepath.Join(root, "images", "rootfs.ext4"))
	kernelPath := envOr("LISAVM_KERNEL_PATH", filepath.Join(root, "vmlinux"))
	fcBin := envOr("LISAVM_FIRECRACKER_BIN", "firecracker")
	apiToken := os.Getenv("LISAVM_API_TOKEN")
	vcpuCount := envInt("LISAVM_VCPU_COUNT", 1)
	memSizeMiB := envInt("LISAVM_MEM_SIZE_MIB", 512)

	st, err := store.New(root)
	if err != nil {
		log.Fatalf("init store: %v", err)
	}
	drv := firecracker.NewDriver(firecracker.Config{
		BinaryPath: fcBin,
		KernelPath: kernelPath,
		VCPUCount:  vcpuCount,
		MemSizeMiB: memSizeMiB,
	})
	prov := host.NewLinuxProvisioner()

	mgr, err := manager.New(manager.Config{
		RootDir:           root,
		RootfsPath:        rootfsPath,
		BridgeName:        bridge,
		GatewayIP:         gatewayIP,
		ReaperInterval:    time.Second,
		SchedulerInterval: time.Second,
	}, st, drv, prov)
	if err != nil {
		log.Fatalf("init manager: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mgr.StartLoops(ctx)

	srv := &http.Server{
		Addr:    listenAddr,
		Handler: server.New(mgr, apiToken),
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("lisavm listening on %s (root=%s bridge=%s)", listenAddr, root, bridge)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("http server: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
