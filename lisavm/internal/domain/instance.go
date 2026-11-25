package domain

import (
	"fmt"
	"time"
)

type State string

const (
	StateStopped  State = "stopped"
	StateBooting  State = "booting"
	StateRunning  State = "running"
	StateSleeping State = "sleeping"
	StateWaking   State = "waking"
	StateDeleting State = "deleting"
)

var validTransitions = map[State]map[State]bool{
	StateStopped: {
		StateBooting:  true,
		StateDeleting: true,
	},
	StateBooting: {
		StateRunning:  true,
		StateStopped:  true,
		StateDeleting: true,
	},
	StateRunning: {
		StateSleeping: true,
		StateStopped:  true,
		StateDeleting: true,
	},
	StateSleeping: {
		StateWaking:   true,
		StateBooting:  true,
		StateDeleting: true,
	},
	StateWaking: {
		StateRunning:  true,
		StateSleeping: true,
		StateStopped:  true,
		StateDeleting: true,
	},
	StateDeleting: {},
}

func (s State) CanTransitionTo(next State) bool {
	if s == next {
		return true
	}
	allowed, ok := validTransitions[s]
	if !ok {
		return false
	}
	return allowed[next]
}

func (s State) ValidateTransition(next State) error {
	if !s.CanTransitionTo(next) {
		return fmt.Errorf("invalid state transition: %s -> %s", s, next)
	}
	return nil
}

type Schedule struct {
	ID        string    `json:"id"`
	Cron      string    `json:"cron"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	Body      string    `json:"body,omitempty"`
	Enabled   bool      `json:"enabled"`
	LastRunAt time.Time `json:"last_run_at,omitempty"`
}

type BootstrapConfig struct {
	Env     map[string]string         `json:"env,omitempty"`
	Plugins map[string]map[string]any `json:"plugins,omitempty"`
}

type Instance struct {
	ID             string                    `json:"id"`
	OwnerID        string                    `json:"owner_id"`
	Name           string                    `json:"name"`
	State          State                     `json:"state"`
	IP             string                    `json:"ip"`
	GatewayIP      string                    `json:"gateway_ip"`
	MAC            string                    `json:"mac"`
	TAP            string                    `json:"tap"`
	DiskPath       string                    `json:"disk_path"`
	SocketPath     string                    `json:"socket_path"`
	SnapshotPath   string                    `json:"snapshot_path,omitempty"`
	MemFilePath    string                    `json:"mem_file_path,omitempty"`
	IdleTimeoutSec int                       `json:"idle_timeout_sec"`
	Plugins        map[string]map[string]any `json:"plugins,omitempty"`
	Env            map[string]string         `json:"env,omitempty"`
	Schedules      []Schedule                `json:"schedules,omitempty"`
	PID            int                       `json:"pid,omitempty"`
	LastActivityAt time.Time                 `json:"last_activity_at"`
	CreatedAt      time.Time                 `json:"created_at"`
	UpdatedAt      time.Time                 `json:"updated_at"`
}

type CreateInstanceRequest struct {
	OwnerID        string                    `json:"owner_id"`
	Name           string                    `json:"name"`
	IdleTimeoutSec int                       `json:"idle_timeout_sec"`
	Plugins        map[string]map[string]any `json:"plugins,omitempty"`
	Env            map[string]string         `json:"env,omitempty"`
	Schedules      []Schedule                `json:"schedules,omitempty"`
	Boot           *bool                     `json:"boot,omitempty"`
}

type UpdateInstanceRequest struct {
	IdleTimeoutSec *int        `json:"idle_timeout_sec,omitempty"`
	Schedules      *[]Schedule `json:"schedules,omitempty"`
}
