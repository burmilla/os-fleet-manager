package server

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/burmilla/os-fleet-manager/internal/fleet"
)

// SwarmStatus is the Docker Swarm state as seen by the reporting server.
// The node counters are only filled in by managers.
type SwarmStatus struct {
	NodeID            string `json:"node_id"`
	ClusterID         string `json:"cluster_id"`
	LocalNodeState    string `json:"local_node_state"`
	Manager           bool   `json:"manager"`
	Nodes             int    `json:"nodes"`
	NodesReady        int    `json:"nodes_ready"`
	Managers          int    `json:"managers"`
	ManagersReachable int    `json:"managers_reachable"`
	// Service counters, managers only. ServicesNotReady lists services
	// which do not run all desired tasks or are in the middle of an update.
	Services         int            `json:"services"`
	ServicesReady    int            `json:"services_ready"`
	ServicesNotReady []ServiceState `json:"services_not_ready,omitempty"`
}

type ServiceState struct {
	Name         string `json:"name"`
	Mode         string `json:"mode"`
	DesiredTasks int    `json:"desired_tasks"`
	RunningTasks int    `json:"running_tasks"`
	UpdateState  string `json:"update_state"`
}

// ContainerStatus covers the local User Docker containers of a server
// which Docker restarts on its own (restart policy always, unless-stopped
// or on-failure). Swarm tasks are not included.
type ContainerStatus struct {
	Containers         int              `json:"containers"`
	ContainersReady    int              `json:"containers_ready"`
	ContainersNotReady []ContainerState `json:"containers_not_ready,omitempty"`
}

type ContainerState struct {
	Name          string `json:"name"`
	State         string `json:"state"`
	Health        string `json:"health"`
	RestartPolicy string `json:"restart_policy"`
	ExitCode      int    `json:"exit_code"`
}

// NomadStatus is the local Nomad client node of a Nomad QEMU node.
type NomadStatus struct {
	NodeID      string `json:"node_id"`
	Status      string `json:"status"`
	Eligibility string `json:"eligibility"`
	Drain       bool   `json:"drain"`
	// Allocations is the number of running allocations on the node.
	Allocations int `json:"allocations"`
}

// RAIDArray is a Linux software RAID (md) array of a server, as shown in
// /proc/mdstat.
type RAIDArray struct {
	Name  string `json:"name"`
	Level string `json:"level"`
	// State is active or inactive.
	State    string `json:"state"`
	ReadOnly bool   `json:"read_only,omitempty"`
	// Devices is the number of member devices the array should have and
	// ActiveDevices the number of them which are in sync.
	Devices       int      `json:"devices"`
	ActiveDevices int      `json:"active_devices"`
	Degraded      bool     `json:"degraded"`
	FailedDevices []string `json:"failed_devices,omitempty"`
	SpareDevices  []string `json:"spare_devices,omitempty"`
	// SyncAction is recovery, resync, check, repair or reshape while one is
	// running or waiting, SyncProgress is in percent.
	SyncAction   string  `json:"sync_action,omitempty"`
	SyncProgress float64 `json:"sync_progress,omitempty"`
}

// Report is the body of POST /api/v1/report.
type Report struct {
	Hostname    string `json:"hostname"`
	Swarm       string `json:"swarm"`
	Environment string `json:"environment"`
	// NodeType is empty for BurmillaOS, fleet.NodeTypeNomadQEMU for Nomad
	// QEMU nodes.
	NodeType      string `json:"node_type,omitempty"`
	OSVersion     string `json:"os_version"`
	DockerVersion string `json:"docker_version"`
	KernelVersion string `json:"kernel_version"`
	// QEMUVersion and NomadVersion are reported by Nomad QEMU nodes.
	QEMUVersion   string       `json:"qemu_version,omitempty"`
	NomadVersion  string       `json:"nomad_version,omitempty"`
	ClientVersion string       `json:"client_version"`
	BootTime      time.Time    `json:"boot_time"`
	State         string       `json:"state"`
	Message       string       `json:"message"`
	SwarmStatus   *SwarmStatus `json:"swarm_status,omitempty"`
	// Containers is omitted when User Docker is not reachable.
	Containers *ContainerStatus `json:"containers,omitempty"`
	// SSH is the applied state: the keys managed by fleet-client and
	// whether sshd accepts passwords.
	SSH *fleet.SSH `json:"ssh,omitempty"`
	// Nomad is omitted when the client could not read it, and by
	// BurmillaOS servers.
	Nomad *NomadStatus `json:"nomad,omitempty"`
	// RAID lists the software RAID arrays, omitted when there are none.
	RAID []RAIDArray `json:"raid,omitempty"`
}

type record struct {
	Report     Report    `json:"report"`
	LastSeen   time.Time `json:"last_seen"`
	RemoteAddr string    `json:"remote_addr"`
}

// Lock is a maintenance lock held by a server.
type Lock struct {
	Group   string    `json:"group"`
	Reason  string    `json:"reason"`
	Expires time.Time `json:"expires"`
}

// Store keeps the last report of each server and the maintenance locks.
// Both are persisted to JSON files in the data directory so that they
// survive a restart of the service.
type Store struct {
	mu        sync.Mutex
	path      string
	locksPath string
	records   map[string]*record
	locks     map[string]Lock // hostname -> lock
	lockTTL   time.Duration
}

func NewStore(dataDir string, lockTTL time.Duration) *Store {
	s := &Store{
		records: map[string]*record{},
		locks:   map[string]Lock{},
		lockTTL: lockTTL,
	}
	if dataDir != "" {
		s.path = filepath.Join(dataDir, "reports.json")
		s.locksPath = filepath.Join(dataDir, "locks.json")
		load(s.path, &s.records)
		load(s.locksPath, &s.locks)
		if s.records == nil {
			s.records = map[string]*record{}
		}
		if s.locks == nil {
			s.locks = map[string]Lock{}
		}
	}
	return s
}

func load(path string, v any) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if err := json.Unmarshal(data, v); err != nil {
		log.Printf("ignoring unreadable %s: %v", path, err)
	}
}

// save writes v atomically: a crash leaves either the old or the new file.
func save(path string, v any) error {
	if path == "" {
		return nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Store) saveLocked() error {
	return save(s.path, s.records)
}

func (s *Store) Put(r Report, remoteAddr string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[r.Hostname] = &record{Report: r, LastSeen: now, RemoteAddr: remoteAddr}
	return s.saveLocked()
}

func (s *Store) Records() map[string]record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordsLocked()
}

func (s *Store) recordsLocked() map[string]record {
	out := make(map[string]record, len(s.records))
	for k, v := range s.records {
		out[k] = *v
	}
	return out
}

func (s *Store) expireLocked(now time.Time) {
	for h, l := range s.locks {
		if !now.Before(l.Expires) {
			delete(s.locks, h)
		}
	}
}

// Acquire takes (or renews) a maintenance lock for hostname in group.
// At most max hosts can hold a lock in the same group. check is called
// with the store locked, on copies of the locks and reports, so that the decision is atomic; a non-nil error
// refuses the lock.
func (s *Store) Acquire(hostname, group, reason string, max int, now time.Time, check func(locks map[string]Lock, records map[string]record) error) (time.Time, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(now)

	if l, ok := s.locks[hostname]; ok && l.Group != group {
		// Server moved to another group, drop the old lock.
		delete(s.locks, hostname)
	}
	holders := s.holdersLocked(group)
	if _, held := s.locks[hostname]; !held {
		if len(holders) >= max {
			return time.Time{}, holders, fmt.Errorf("%d of %d maintenance slots in use", len(holders), max)
		}
		if check != nil {
			if err := check(s.copyLocked(), s.recordsLocked()); err != nil {
				return time.Time{}, holders, err
			}
		}
	}
	expires := now.Add(s.lockTTL)
	prev, had := s.locks[hostname]
	s.locks[hostname] = Lock{Group: group, Reason: reason, Expires: expires}
	if err := save(s.locksPath, s.locks); err != nil {
		// A lock that would be lost on restart is not granted.
		if had {
			s.locks[hostname] = prev
		} else {
			delete(s.locks, hostname)
		}
		return time.Time{}, holders, fmt.Errorf("persist maintenance lock: %w", err)
	}
	return expires, s.holdersLocked(group), nil
}

func (s *Store) holdersLocked(group string) []string {
	holders := []string{}
	for h, l := range s.locks {
		if l.Group == group {
			holders = append(holders, h)
		}
	}
	sort.Strings(holders)
	return holders
}

func (s *Store) copyLocked() map[string]Lock {
	out := make(map[string]Lock, len(s.locks))
	for h, l := range s.locks {
		out[h] = l
	}
	return out
}

func (s *Store) Release(hostname string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.locks[hostname]
	if !ok {
		return false
	}
	delete(s.locks, hostname)
	if err := save(s.locksPath, s.locks); err != nil {
		log.Printf("failed to persist maintenance locks: %v", err)
	}
	return true
}

// Locks returns the current locks by hostname.
func (s *Store) Locks(now time.Time) map[string]Lock {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(now)
	return s.copyLocked()
}
