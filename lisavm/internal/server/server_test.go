package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/softprobe/lisa/lisavm/internal/domain"
	"github.com/softprobe/lisa/lisavm/internal/manager"
	"github.com/softprobe/lisa/lisavm/internal/store"
)

type stubDriver struct{}

func (stubDriver) Boot(_ context.Context, _ *domain.Instance) (int, error) { return 4242, nil }
func (stubDriver) PauseAndSnapshot(_ context.Context, _ *domain.Instance, snap, mem string) error {
	_ = os.MkdirAll(filepath.Dir(snap), 0o755)
	_ = os.WriteFile(snap, []byte("s"), 0o644)
	return os.WriteFile(mem, []byte("m"), 0o644)
}
func (stubDriver) RestoreFromSnapshot(_ context.Context, _ *domain.Instance, _, _ string) (int, error) {
	return 4243, nil
}
func (stubDriver) Stop(_ context.Context, _ *domain.Instance) error         { return nil }
func (stubDriver) WaitHealthy(_ context.Context, _ string) error            { return nil }
func (stubDriver) IsGuestBusy(_ context.Context, _ string) (bool, error)    { return false, nil }

type stubProv struct{}

func (stubProv) CloneDisk(_, dst string) error {
	_ = os.MkdirAll(filepath.Dir(dst), 0o755)
	return os.WriteFile(dst, []byte("d"), 0o644)
}
func (stubProv) InjectBootstrap(_ string, _ domain.BootstrapConfig) error { return nil }
func (stubProv) EnsureTAP(_, _ string) error                              { return nil }
func (stubProv) DeleteTAP(_ string) error                                 { return nil }

func TestServerAuthAndCRUD(t *testing.T) {
	root := t.TempDir()
	st, err := store.New(root)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := manager.New(manager.Config{
		RootDir:    root,
		RootfsPath: filepath.Join(root, "rootfs.ext4"),
		Now:        time.Now,
	}, st, stubDriver{}, stubProv{})
	if err != nil {
		t.Fatal(err)
	}

	srv := New(mgr, "secret-token")

	// Health is unauthenticated.
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /health, got %d", rec.Code)
	}

	// /v1/* requires bearer token when configured.
	req = httptest.NewRequest(http.MethodGet, "/v1/instances", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}

	// Create instance with valid token.
	body := bytes.NewBufferString(`{"owner_id":"ws-1","name":"vm-a","idle_timeout_sec":10}`)
	req = httptest.NewRequest(http.MethodPost, "/v1/instances", body)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created domain.Instance
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	// Patch idle_timeout_sec.
	req = httptest.NewRequest(http.MethodPatch, "/v1/instances/"+created.ID, bytes.NewBufferString(`{"idle_timeout_sec":30}`))
	req.Header.Set("Authorization", "Bearer secret-token")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on PATCH, got %d", rec.Code)
	}

	// Sleep & Wake.
	req = httptest.NewRequest(http.MethodPost, "/v1/instances/"+created.ID+"/sleep", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on sleep, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/instances/"+created.ID+"/wake", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on wake, got %d", rec.Code)
	}

	// Delete.
	req = httptest.NewRequest(http.MethodDelete, "/v1/instances/"+created.ID, nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 on delete, got %d", rec.Code)
	}
}
