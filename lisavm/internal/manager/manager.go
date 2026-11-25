package manager

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/softprobe/lisa/lisavm/internal/cron"
	"github.com/softprobe/lisa/lisavm/internal/domain"
	"github.com/softprobe/lisa/lisavm/internal/firecracker"
	"github.com/softprobe/lisa/lisavm/internal/host"
	"github.com/softprobe/lisa/lisavm/internal/store"
)

var (
	ErrNotFound = errors.New("instance not found")
	ErrConflict = errors.New("instance state conflict")
	ErrInvalid  = errors.New("invalid request")
)

type Config struct {
	RootDir           string
	RootfsPath        string
	BridgeName        string
	GatewayIP         string
	ReaperInterval    time.Duration
	SchedulerInterval time.Duration
	Now               func() time.Time
}

type entry struct {
	mu   sync.Mutex
	inst *domain.Instance
}

type Manager struct {
	cfg    Config
	store  *store.Store
	driver firecracker.VMDriver
	host   host.Provisioner
	client *http.Client

	mu      sync.RWMutex
	entries map[string]*entry
}

func New(cfg Config, st *store.Store, drv firecracker.VMDriver, prov host.Provisioner) (*Manager, error) {
	if cfg.BridgeName == "" {
		cfg.BridgeName = "lisabr0"
	}
	if cfg.GatewayIP == "" {
		cfg.GatewayIP = "172.16.0.1"
	}
	if cfg.ReaperInterval <= 0 {
		cfg.ReaperInterval = time.Second
	}
	if cfg.SchedulerInterval <= 0 {
		cfg.SchedulerInterval = time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	m := &Manager{
		cfg:     cfg,
		store:   st,
		driver:  drv,
		host:    prov,
		client:  &http.Client{Timeout: 30 * time.Second},
		entries: make(map[string]*entry),
	}

	if err := m.loadAndReconcile(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) loadAndReconcile() error {
	loaded, err := m.store.LoadAll()
	if err != nil {
		return err
	}
	for _, inst := range loaded {
		switch inst.State {
		case domain.StateDeleting:
			_ = m.driver.Stop(context.Background(), inst)
			_ = m.host.DeleteTAP(inst.TAP)
			_ = m.store.Delete(inst.ID)
			continue
		case domain.StateBooting:
			_ = m.driver.Stop(context.Background(), inst)
			_ = m.host.DeleteTAP(inst.TAP)
			inst.State = domain.StateStopped
			inst.PID = 0
		case domain.StateWaking:
			_ = m.driver.Stop(context.Background(), inst)
			_ = m.host.DeleteTAP(inst.TAP)
			if filesExist(inst.SnapshotPath, inst.MemFilePath) {
				inst.State = domain.StateSleeping
			} else {
				inst.State = domain.StateStopped
			}
			inst.PID = 0
		case domain.StateRunning:
			if !processAlive(inst.PID) {
				_ = m.host.DeleteTAP(inst.TAP)
				if filesExist(inst.SnapshotPath, inst.MemFilePath) {
					inst.State = domain.StateSleeping
				} else {
					inst.State = domain.StateStopped
				}
				inst.PID = 0
			}
		}
		if err := m.store.Save(inst); err != nil {
			return err
		}
		m.entries[inst.ID] = &entry{inst: inst}
	}
	return nil
}

func (m *Manager) StartLoops(ctx context.Context) {
	go m.reaperLoop(ctx)
	go m.schedulerLoop(ctx)
}

func (m *Manager) reaperLoop(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.ReaperInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.ReapIdleOnce(ctx)
		}
	}
}

func (m *Manager) schedulerLoop(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.SchedulerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.RunDueSchedulesOnce(ctx)
		}
	}
}

func (m *Manager) Create(ctx context.Context, req domain.CreateInstanceRequest) (*domain.Instance, error) {
	if strings.TrimSpace(req.OwnerID) == "" {
		return nil, fmt.Errorf("%w: owner_id is required", ErrInvalid)
	}
	if strings.TrimSpace(req.Name) == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalid)
	}
	if req.IdleTimeoutSec < 0 {
		return nil, fmt.Errorf("%w: idle_timeout_sec must be >= 0", ErrInvalid)
	}
	if err := validateSchedules(req.Schedules); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}

	id := newInstanceID()
	m.mu.Lock()
	ip, mac, err := m.allocateIPLocked()
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	tap := "lvt-" + strings.TrimPrefix(id, "lisa-")
	if len(tap) > 15 {
		tap = tap[:15]
	}

	now := m.cfg.Now().UTC()
	instDir := m.store.InstanceDir(id)
	inst := &domain.Instance{
		ID:             id,
		OwnerID:        req.OwnerID,
		Name:           req.Name,
		State:          domain.StateStopped,
		IP:             ip,
		GatewayIP:      m.cfg.GatewayIP,
		MAC:            mac,
		TAP:            tap,
		DiskPath:       filepath.Join(instDir, "disk.ext4"),
		SocketPath:     filepath.Join(instDir, "firecracker.sock"),
		IdleTimeoutSec: req.IdleTimeoutSec,
		Plugins:        clonePlugins(req.Plugins),
		Env:            cloneEnv(req.Env),
		Schedules:      cloneSchedules(req.Schedules),
		LastActivityAt: now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	ent := &entry{inst: inst}
	ent.mu.Lock()
	m.entries[id] = ent
	m.mu.Unlock()

	cleanup := func() {
		m.mu.Lock()
		delete(m.entries, id)
		m.mu.Unlock()
		_ = m.store.Delete(id)
		ent.mu.Unlock()
	}

	if err := m.host.CloneDisk(m.cfg.RootfsPath, inst.DiskPath); err != nil {
		cleanup()
		return nil, err
	}
	if err := m.host.InjectBootstrap(inst.DiskPath, domain.BootstrapConfig{
		Env:     inst.Env,
		Plugins: inst.Plugins,
	}); err != nil {
		cleanup()
		return nil, err
	}
	if err := m.store.Save(inst); err != nil {
		cleanup()
		return nil, err
	}
	ent.mu.Unlock()

	shouldBoot := req.Boot == nil || *req.Boot
	if !shouldBoot {
		return cloneInstance(inst), nil
	}
	return m.EnsureRunning(ctx, id)
}

func (m *Manager) List(ownerID string) []*domain.Instance {
	m.mu.RLock()
	entries := make([]*entry, 0, len(m.entries))
	for _, ent := range m.entries {
		entries = append(entries, ent)
	}
	m.mu.RUnlock()

	var out []*domain.Instance
	for _, ent := range entries {
		ent.mu.Lock()
		if ownerID == "" || ent.inst.OwnerID == ownerID {
			out = append(out, cloneInstance(ent.inst))
		}
		ent.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func (m *Manager) Get(id string) (*domain.Instance, error) {
	ent, err := m.getEntry(id)
	if err != nil {
		return nil, err
	}
	ent.mu.Lock()
	defer ent.mu.Unlock()
	return cloneInstance(ent.inst), nil
}

func (m *Manager) Update(id string, req domain.UpdateInstanceRequest) (*domain.Instance, error) {
	if req.IdleTimeoutSec != nil && *req.IdleTimeoutSec < 0 {
		return nil, fmt.Errorf("%w: idle_timeout_sec must be >= 0", ErrInvalid)
	}
	if req.Schedules != nil {
		if err := validateSchedules(*req.Schedules); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}

	ent, err := m.getEntry(id)
	if err != nil {
		return nil, err
	}
	ent.mu.Lock()
	defer ent.mu.Unlock()

	if ent.inst.State == domain.StateDeleting {
		return nil, ErrConflict
	}
	if req.IdleTimeoutSec != nil {
		ent.inst.IdleTimeoutSec = *req.IdleTimeoutSec
	}
	if req.Schedules != nil {
		ent.inst.Schedules = cloneSchedules(*req.Schedules)
	}
	ent.inst.UpdatedAt = m.cfg.Now().UTC()
	if err := m.store.Save(ent.inst); err != nil {
		return nil, err
	}
	return cloneInstance(ent.inst), nil
}

func (m *Manager) Delete(ctx context.Context, id string) error {
	ent, err := m.getEntry(id)
	if err != nil {
		return err
	}
	ent.mu.Lock()
	defer ent.mu.Unlock()

	if err := ent.inst.State.ValidateTransition(domain.StateDeleting); err != nil {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	ent.inst.State = domain.StateDeleting
	ent.inst.UpdatedAt = m.cfg.Now().UTC()
	_ = m.store.Save(ent.inst)

	_ = m.driver.Stop(ctx, ent.inst)
	_ = m.host.DeleteTAP(ent.inst.TAP)

	m.mu.Lock()
	delete(m.entries, id)
	m.mu.Unlock()

	return m.store.Delete(id)
}

func (m *Manager) EnsureRunning(ctx context.Context, id string) (*domain.Instance, error) {
	ent, err := m.getEntry(id)
	if err != nil {
		return nil, err
	}
	ent.mu.Lock()
	defer ent.mu.Unlock()

	inst := ent.inst
	switch inst.State {
	case domain.StateRunning:
		inst.LastActivityAt = m.cfg.Now().UTC()
		inst.UpdatedAt = inst.LastActivityAt
		_ = m.store.Save(inst)
		return cloneInstance(inst), nil
	case domain.StateDeleting:
		return nil, fmt.Errorf("%w: instance is deleting", ErrConflict)
	}

	canRestore := inst.State == domain.StateSleeping && filesExist(inst.SnapshotPath, inst.MemFilePath)
	if canRestore {
		if err := inst.State.ValidateTransition(domain.StateWaking); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrConflict, err)
		}
		inst.State = domain.StateWaking
		inst.UpdatedAt = m.cfg.Now().UTC()
		if err := m.store.Save(inst); err != nil {
			return nil, err
		}

		if err := m.host.EnsureTAP(inst.TAP, m.cfg.BridgeName); err != nil {
			inst.State = domain.StateSleeping
			_ = m.store.Save(inst)
			return nil, err
		}

		pid, err := m.driver.RestoreFromSnapshot(ctx, inst, inst.SnapshotPath, inst.MemFilePath)
		if err != nil {
			_ = m.host.DeleteTAP(inst.TAP)
			inst.State = domain.StateSleeping
			inst.PID = 0
			_ = m.store.Save(inst)
			return nil, err
		}
		inst.PID = pid
		if err := m.driver.WaitHealthy(ctx, inst.IP); err != nil {
			_ = m.driver.Stop(ctx, inst)
			_ = m.host.DeleteTAP(inst.TAP)
			inst.State = domain.StateSleeping
			inst.PID = 0
			_ = m.store.Save(inst)
			return nil, err
		}

		now := m.cfg.Now().UTC()
		inst.State = domain.StateRunning
		inst.LastActivityAt = now
		inst.UpdatedAt = now
		if err := m.store.Save(inst); err != nil {
			return nil, err
		}
		return cloneInstance(inst), nil
	}

	if err := inst.State.ValidateTransition(domain.StateBooting); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	inst.State = domain.StateBooting
	inst.UpdatedAt = m.cfg.Now().UTC()
	if err := m.store.Save(inst); err != nil {
		return nil, err
	}

	if err := m.host.EnsureTAP(inst.TAP, m.cfg.BridgeName); err != nil {
		inst.State = domain.StateStopped
		_ = m.store.Save(inst)
		return nil, err
	}

	pid, err := m.driver.Boot(ctx, inst)
	if err != nil {
		_ = m.host.DeleteTAP(inst.TAP)
		inst.State = domain.StateStopped
		inst.PID = 0
		_ = m.store.Save(inst)
		return nil, err
	}
	inst.PID = pid
	if err := m.driver.WaitHealthy(ctx, inst.IP); err != nil {
		_ = m.driver.Stop(ctx, inst)
		_ = m.host.DeleteTAP(inst.TAP)
		inst.State = domain.StateStopped
		inst.PID = 0
		_ = m.store.Save(inst)
		return nil, err
	}

	now := m.cfg.Now().UTC()
	inst.State = domain.StateRunning
	inst.LastActivityAt = now
	inst.UpdatedAt = now
	if err := m.store.Save(inst); err != nil {
		return nil, err
	}
	return cloneInstance(inst), nil
}

func (m *Manager) Sleep(ctx context.Context, id string) (*domain.Instance, error) {
	ent, err := m.getEntry(id)
	if err != nil {
		return nil, err
	}
	ent.mu.Lock()
	defer ent.mu.Unlock()

	return m.sleepLocked(ctx, ent.inst)
}

func (m *Manager) sleepLocked(ctx context.Context, inst *domain.Instance) (*domain.Instance, error) {
	if inst.State == domain.StateSleeping {
		return cloneInstance(inst), nil
	}
	if inst.State != domain.StateRunning {
		return nil, fmt.Errorf("%w: cannot sleep instance in state %s", ErrConflict, inst.State)
	}

	snapDir := m.store.SnapshotDir(inst.ID)
	snapPath := filepath.Join(snapDir, "vmstate.snap")
	memPath := filepath.Join(snapDir, "mem.bin")

	if err := m.driver.PauseAndSnapshot(ctx, inst, snapPath, memPath); err != nil {
		return nil, err
	}
	if err := m.host.DeleteTAP(inst.TAP); err != nil {
		log.Printf("warning: delete tap %s after sleep: %v", inst.TAP, err)
	}

	inst.State = domain.StateSleeping
	inst.PID = 0
	inst.SnapshotPath = snapPath
	inst.MemFilePath = memPath
	inst.UpdatedAt = m.cfg.Now().UTC()
	if err := m.store.Save(inst); err != nil {
		return nil, err
	}
	return cloneInstance(inst), nil
}

func (m *Manager) TouchActivity(id string) {
	ent, err := m.getEntry(id)
	if err != nil {
		return
	}
	ent.mu.Lock()
	defer ent.mu.Unlock()
	if ent.inst.State == domain.StateRunning {
		now := m.cfg.Now().UTC()
		ent.inst.LastActivityAt = now
		ent.inst.UpdatedAt = now
		_ = m.store.Save(ent.inst)
	}
}

func (m *Manager) ReapIdleOnce(ctx context.Context) {
	m.mu.RLock()
	entries := make([]*entry, 0, len(m.entries))
	for _, ent := range m.entries {
		entries = append(entries, ent)
	}
	m.mu.RUnlock()

	now := m.cfg.Now().UTC()
	for _, ent := range entries {
		ent.mu.Lock()
		inst := ent.inst
		if inst.State != domain.StateRunning || inst.IdleTimeoutSec <= 0 {
			ent.mu.Unlock()
			continue
		}
		if now.Sub(inst.LastActivityAt) < time.Duration(inst.IdleTimeoutSec)*time.Second {
			ent.mu.Unlock()
			continue
		}
		busy, err := m.driver.IsGuestBusy(ctx, inst.IP)
		if err != nil || busy {
			ent.mu.Unlock()
			continue
		}
		if _, err := m.sleepLocked(ctx, inst); err != nil {
			log.Printf("idle reaper failed to sleep %s: %v", inst.ID, err)
		}
		ent.mu.Unlock()
	}
}

func (m *Manager) RunDueSchedulesOnce(ctx context.Context) {
	m.mu.RLock()
	entries := make([]*entry, 0, len(m.entries))
	for _, ent := range m.entries {
		entries = append(entries, ent)
	}
	m.mu.RUnlock()

	type dueJob struct {
		instanceID string
		sched      domain.Schedule
	}
	var due []dueJob
	now := m.cfg.Now().UTC()

	for _, ent := range entries {
		ent.mu.Lock()
		inst := ent.inst
		if inst.State == domain.StateDeleting {
			ent.mu.Unlock()
			continue
		}
		changed := false
		for i := range inst.Schedules {
			s := &inst.Schedules[i]
			if !s.Enabled {
				continue
			}
			spec, err := cron.Parse(s.Cron)
			if err != nil {
				continue
			}
			if spec.Due(now, s.LastRunAt) {
				s.LastRunAt = now
				changed = true
				due = append(due, dueJob{instanceID: inst.ID, sched: *s})
			}
		}
		if changed {
			inst.UpdatedAt = now
			_ = m.store.Save(inst)
		}
		ent.mu.Unlock()
	}

	for _, job := range due {
		go m.executeScheduledJob(ctx, job.instanceID, job.sched)
	}
}

func (m *Manager) executeScheduledJob(ctx context.Context, instanceID string, sched domain.Schedule) {
	inst, err := m.EnsureRunning(ctx, instanceID)
	if err != nil {
		log.Printf("schedule %s for %s failed to wake instance: %v", sched.ID, instanceID, err)
		return
	}
	method := strings.ToUpper(strings.TrimSpace(sched.Method))
	if method == "" {
		method = http.MethodPost
	}
	path := sched.Path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	targetURL := fmt.Sprintf("http://%s:8000%s", inst.IP, path)

	var bodyReader io.Reader
	if sched.Body != "" {
		bodyReader = bytes.NewBufferString(sched.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, targetURL, bodyReader)
	if err != nil {
		log.Printf("schedule %s for %s invalid request: %v", sched.ID, instanceID, err)
		return
	}
	if sched.Body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.client.Do(req)
	if err != nil {
		log.Printf("schedule %s for %s request failed: %v", sched.ID, instanceID, err)
		return
	}
	_ = resp.Body.Close()
	log.Printf("schedule %s for %s -> %d", sched.ID, instanceID, resp.StatusCode)
	m.TouchActivity(instanceID)
}

func (m *Manager) getEntry(id string) (*entry, error) {
	m.mu.RLock()
	ent, ok := m.entries[id]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrNotFound
	}
	return ent, nil
}

func (m *Manager) allocateIPLocked() (string, string, error) {
	used := map[string]bool{
		m.cfg.GatewayIP: true,
		"172.16.0.10":   true,
	}
	for _, ent := range m.entries {
		used[ent.inst.IP] = true
	}
	for x := 0; x <= 254; x++ {
		for y := 2; y <= 254; y++ {
			ip := fmt.Sprintf("172.16.%d.%d", x, y)
			if !used[ip] {
				mac := fmt.Sprintf("06:00:AC:10:%02X:%02X", x, y)
				return ip, mac, nil
			}
		}
	}
	return "", "", fmt.Errorf("no available IPs in 172.16.0.0/16")
}

func validateSchedules(schedules []domain.Schedule) error {
	for i, s := range schedules {
		if strings.TrimSpace(s.ID) == "" {
			return fmt.Errorf("schedule[%d].id is required", i)
		}
		if _, err := cron.Parse(s.Cron); err != nil {
			return fmt.Errorf("schedule[%d].cron: %w", i, err)
		}
		if strings.TrimSpace(s.Path) == "" {
			return fmt.Errorf("schedule[%d].path is required", i)
		}
	}
	return nil
}

func newInstanceID() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return "lisa-" + hex.EncodeToString(b[:])
}

func filesExist(paths ...string) bool {
	for _, p := range paths {
		if p == "" {
			return false
		}
		info, err := os.Stat(p)
		if err != nil || info.IsDir() || info.Size() == 0 {
			return false
		}
	}
	return true
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func cloneInstance(inst *domain.Instance) *domain.Instance {
	cp := *inst
	cp.Env = cloneEnv(inst.Env)
	cp.Plugins = clonePlugins(inst.Plugins)
	cp.Schedules = cloneSchedules(inst.Schedules)
	return &cp
}

func cloneEnv(src map[string]string) map[string]string {
	if src == nil {
		return nil
	}
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func clonePlugins(src map[string]map[string]any) map[string]map[string]any {
	if src == nil {
		return nil
	}
	out := make(map[string]map[string]any, len(src))
	for k, v := range src {
		inner := make(map[string]any, len(v))
		for ik, iv := range v {
			inner[ik] = iv
		}
		out[k] = inner
	}
	return out
}

func cloneSchedules(src []domain.Schedule) []domain.Schedule {
	if src == nil {
		return nil
	}
	out := make([]domain.Schedule, len(src))
	copy(out, src)
	return out
}
