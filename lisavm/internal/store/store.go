package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/softprobe/lisa/lisavm/internal/domain"
)

type Store struct {
	root string
}

func New(root string) (*Store, error) {
	instancesDir := filepath.Join(root, "instances")
	if err := os.MkdirAll(instancesDir, 0o700); err != nil {
		return nil, fmt.Errorf("create instances dir: %w", err)
	}
	_ = os.Chmod(instancesDir, 0o700)
	snapshotsDir := filepath.Join(root, "snapshots")
	if err := os.MkdirAll(snapshotsDir, 0o700); err != nil {
		return nil, fmt.Errorf("create snapshots dir: %w", err)
	}
	_ = os.Chmod(snapshotsDir, 0o700)
	return &Store{root: root}, nil
}

func (s *Store) InstanceDir(id string) string {
	return filepath.Join(s.root, "instances", id)
}

func (s *Store) SnapshotDir(id string) string {
	return filepath.Join(s.root, "snapshots", id)
}

func (s *Store) MetaPath(id string) string {
	return filepath.Join(s.InstanceDir(id), "meta.json")
}

func (s *Store) Save(inst *domain.Instance) error {
	dir := s.InstanceDir(inst.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create instance dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("chmod instance dir: %w", err)
	}
	data, err := json.MarshalIndent(inst, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal instance %s: %w", inst.ID, err)
	}
	data = append(data, '\n')
	tmpPath := s.MetaPath(inst.ID) + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("write temp meta for %s: %w", inst.ID, err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod temp meta for %s: %w", inst.ID, err)
	}
	if err := os.Rename(tmpPath, s.MetaPath(inst.ID)); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename meta for %s: %w", inst.ID, err)
	}
	return nil
}

func (s *Store) Delete(id string) error {
	if err := os.RemoveAll(s.SnapshotDir(id)); err != nil {
		return fmt.Errorf("remove snapshot dir for %s: %w", id, err)
	}
	if err := os.RemoveAll(s.InstanceDir(id)); err != nil {
		return fmt.Errorf("remove instance dir for %s: %w", id, err)
	}
	return nil
}

func (s *Store) LoadAll() ([]*domain.Instance, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "instances"))
	if err != nil {
		return nil, fmt.Errorf("read instances dir: %w", err)
	}
	var out []*domain.Instance
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		metaPath := s.MetaPath(entry.Name())
		data, err := os.ReadFile(metaPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", metaPath, err)
		}
		var inst domain.Instance
		if err := json.Unmarshal(data, &inst); err != nil {
			return nil, fmt.Errorf("parse %s: %w", metaPath, err)
		}
		out = append(out, &inst)
	}
	return out, nil
}
