package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
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
}

// Report is the body of POST /api/v1/report.
type Report struct {
	Hostname      string       `json:"hostname"`
	Swarm         string       `json:"swarm"`
	Environment   string       `json:"environment"`
	OSVersion     string       `json:"os_version"`
	DockerVersion string       `json:"docker_version"`
	KernelVersion string       `json:"kernel_version"`
	ClientVersion string       `json:"client_version"`
	BootTime      time.Time    `json:"boot_time"`
	State         string       `json:"state"`
	Message       string       `json:"message"`
	SwarmStatus   *SwarmStatus `json:"swarm_status,omitempty"`
}

type record struct {
	Report     Report    `json:"report"`
	LastSeen   time.Time `json:"last_seen"`
	RemoteAddr string    `json:"remote_addr"`
}

// Lock is a maintenance lock held by a server.
type Lock struct {
	Group   string
	Reason  string
	Expires time.Time
}

// Store keeps the last report of each server and the maintenance locks.
// Reports are persisted to a JSON file so they survive restarts; locks are
// in memory only and expire on their own.
type Store struct {
	mu      sync.Mutex
	path    string
	records map[string]*record
	locks   map[string]Lock // hostname -> lock
	lockTTL time.Duration
}

func NewStore(dataDir string, lockTTL time.Duration) *Store {
	s := &Store{
		records: map[string]*record{},
		locks:   map[string]Lock{},
		lockTTL: lockTTL,
	}
	if dataDir != "" {
		s.path = filepath.Join(dataDir, "reports.json")
		if data, err := os.ReadFile(s.path); err == nil {
			_ = json.Unmarshal(data, &s.records)
		}
	}
	return s
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	data, err := json.Marshal(s.records)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
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
	s.locks[hostname] = Lock{Group: group, Reason: reason, Expires: expires}
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
	delete(s.locks, hostname)
	return ok
}

// Locks returns the current locks by hostname.
func (s *Store) Locks(now time.Time) map[string]Lock {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked(now)
	return s.copyLocked()
}
