package manager

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/softprobe/lisa/lisavm/internal/domain"
	"github.com/softprobe/lisa/lisavm/internal/store"
)

type fakeDriver struct {
	mu           sync.Mutex
	bootCount    int
	restoreCount int
	pauseCount   int
	stopCount    int
	busy         bool
	bootDelay    time.Duration
	nextPID      int
}

func (f *fakeDriver) Boot(ctx context.Context, _ *domain.Instance) (int, error) {
	if f.bootDelay > 0 {
		select {
		case <-time.After(f.bootDelay):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bootCount++
	f.nextPID++
	return 1000 + f.nextPID, nil
}

func (f *fakeDriver) PauseAndSnapshot(_ context.Context, _ *domain.Instance, snapshotPath, memFilePath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pauseCount++
	if err := os.MkdirAll(filepath.Dir(snapshotPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(snapshotPath, []byte("snap"), 0o644); err != nil {
		return err
	}
	return os.WriteFile(memFilePath, []byte("mem"), 0o644)
}

func (f *fakeDriver) RestoreFromSnapshot(_ context.Context, _ *domain.Instance, _, _ string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restoreCount++
	f.nextPID++
	return 2000 + f.nextPID, nil
}

func (f *fakeDriver) Stop(_ context.Context, _ *domain.Instance) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCount++
	return nil
}

func (f *fakeDriver) WaitHealthy(_ context.Context, _ string) error {
	return nil
}

func (f *fakeDriver) IsGuestBusy(_ context.Context, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.busy, nil
}

type fakeProvisioner struct {
	mu           sync.Mutex
	cloned       int
	bootstrapped []domain.BootstrapConfig
	tapsEnsured  []string
	tapsDeleted  []string
}

func (p *fakeProvisioner) CloneDisk(_, dstDisk string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cloned++
	if err := os.MkdirAll(filepath.Dir(dstDisk), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dstDisk, []byte("disk"), 0o644)
}

func (p *fakeProvisioner) InjectBootstrap(_ string, cfg domain.BootstrapConfig) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bootstrapped = append(p.bootstrapped, cfg)
	return nil
}

func (p *fakeProvisioner) EnsureTAP(tapName, _ string, _, _ int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tapsEnsured = append(p.tapsEnsured, tapName)
	return nil
}

func (p *fakeProvisioner) DeleteTAP(tapName string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tapsDeleted = append(p.tapsDeleted, tapName)
	return nil
}

func TestSingleFlightWakeAndSnapshotRestore(t *testing.T) {
	root := t.TempDir()
	st, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	drv := &fakeDriver{bootDelay: 40 * time.Millisecond}
	prov := &fakeProvisioner{}
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	mgr, err := New(Config{
		RootDir:    root,
		RootfsPath: filepath.Join(root, "rootfs.ext4"),
		Now:        func() time.Time { return now },
	}, st, drv, prov)
	if err != nil {
		t.Fatal(err)
	}

	bootFalse := false
	inst, err := mgr.Create(context.Background(), domain.CreateInstanceRequest{
		OwnerID:        "ws-1",
		Name:           "qa-vm",
		IdleTimeoutSec: 5,
		Env:            map[string]string{"LISA_WORKSPACE_ID": "ws-1"},
		Boot:           &bootFalse,
	})
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != domain.StateStopped {
		t.Fatalf("expected stopped, got %s", inst.State)
	}

	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := mgr.EnsureRunning(context.Background(), inst.ID)
			if err != nil {
				t.Errorf("EnsureRunning error: %v", err)
				return
			}
			if got.State != domain.StateRunning {
				t.Errorf("expected running, got %s", got.State)
			}
		}()
	}
	wg.Wait()

	if drv.bootCount != 1 {
		t.Fatalf("expected single-flight bootCount=1, got %d", drv.bootCount)
	}

	slept, err := mgr.Sleep(context.Background(), inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if slept.State != domain.StateSleeping {
		t.Fatalf("expected sleeping, got %s", slept.State)
	}
	if len(prov.tapsDeleted) != 1 {
		t.Fatalf("expected TAP deleted on sleep, got %v", prov.tapsDeleted)
	}

	woken, err := mgr.EnsureRunning(context.Background(), inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if woken.State != domain.StateRunning {
		t.Fatalf("expected running after wake, got %s", woken.State)
	}
	if drv.restoreCount != 1 {
		t.Fatalf("expected restoreCount=1, got %d", drv.restoreCount)
	}
	if len(prov.tapsEnsured) != 2 {
		t.Fatalf("expected TAP re-ensured before snapshot restore, got %v", prov.tapsEnsured)
	}
}

func TestIdleReaperRespectsBusyAndPersistenceAcrossRestart(t *testing.T) {
	root := t.TempDir()
	st, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	drv := &fakeDriver{}
	prov := &fakeProvisioner{}
	cur := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	mgr, err := New(Config{
		RootDir:    root,
		RootfsPath: filepath.Join(root, "rootfs.ext4"),
		Now:        func() time.Time { return cur },
	}, st, drv, prov)
	if err != nil {
		t.Fatal(err)
	}

	inst, err := mgr.Create(context.Background(), domain.CreateInstanceRequest{
		OwnerID:        "ws-1",
		Name:           "reaper-vm",
		IdleTimeoutSec: 4,
	})
	if err != nil {
		t.Fatal(err)
	}

	cur = cur.Add(10 * time.Second)
	drv.busy = true
	mgr.ReapIdleOnce(context.Background())
	got, _ := mgr.Get(inst.ID)
	if got.State != domain.StateRunning {
		t.Fatalf("expected running while guest busy, got %s", got.State)
	}

	drv.busy = false
	mgr.ReapIdleOnce(context.Background())
	got, _ = mgr.Get(inst.ID)
	if got.State != domain.StateSleeping {
		t.Fatalf("expected sleeping when idle and not busy, got %s", got.State)
	}

	mgr2, err := New(Config{
		RootDir:    root,
		RootfsPath: filepath.Join(root, "rootfs.ext4"),
		Now:        func() time.Time { return cur },
	}, st, drv, prov)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := mgr2.Get(inst.ID)
	if err != nil {
		t.Fatalf("expected instance persisted across restart: %v", err)
	}
	if reloaded.State != domain.StateSleeping || reloaded.IP != inst.IP {
		t.Fatalf("unexpected reloaded state: %+v", reloaded)
	}
}

func TestCronSchedulerWakesSleepingVMAndExecutesJob(t *testing.T) {
	var hitCount atomic.Int32
	var gotPath atomic.Value
	guestSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitCount.Add(1)
		gotPath.Store(r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": "true"})
	}))
	defer guestSrv.Close()

	addr := strings.TrimPrefix(guestSrv.URL, "http://")
	hostOnly, portOnly, _ := net.SplitHostPort(addr)

	root := t.TempDir()
	st, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	drv := &fakeDriver{}
	prov := &fakeProvisioner{}
	cur := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

	mgr, err := New(Config{
		RootDir:    root,
		RootfsPath: filepath.Join(root, "rootfs.ext4"),
		Now:        func() time.Time { return cur },
	}, st, drv, prov)
	if err != nil {
		t.Fatal(err)
	}
	mgr.client = &http.Client{
		Transport: &rewritePortTransport{targetHostPort: net.JoinHostPort(hostOnly, portOnly), base: http.DefaultTransport},
	}

	inst, err := mgr.Create(context.Background(), domain.CreateInstanceRequest{
		OwnerID:        "ws-1",
		Name:           "cron-vm",
		IdleTimeoutSec: 4,
		Schedules: []domain.Schedule{
			{
				ID:      "heartbeat",
				Cron:    "@every 5s",
				Method:  http.MethodPost,
				Path:    "/v1/conversations",
				Enabled: true,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Sleep(context.Background(), inst.ID); err != nil {
		t.Fatal(err)
	}

	mgr.RunDueSchedulesOnce(context.Background())

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && hitCount.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if hitCount.Load() != 1 {
		t.Fatalf("expected scheduled job to execute once, got %d", hitCount.Load())
	}
	if gotPath.Load() != "/v1/conversations" {
		t.Fatalf("expected /v1/conversations, got %v", gotPath.Load())
	}
	after, _ := mgr.Get(inst.ID)
	if after.State != domain.StateRunning {
		t.Fatalf("expected sleeping VM woken by cron, got %s", after.State)
	}
}

type rewritePortTransport struct {
	targetHostPort string
	base           http.RoundTripper
}

func (t *rewritePortTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.URL.Host = t.targetHostPort
	return t.base.RoundTrip(cloned)
}
