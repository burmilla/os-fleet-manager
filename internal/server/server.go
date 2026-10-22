// Package server implements the Fleet Manager HTTP API.
package server

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/burmilla/os-fleet-manager/internal/fleet"
)

type Server struct {
	Store *Store
	Now   func() time.Time

	config atomic.Pointer[fleet.Config]

	syncMu       sync.Mutex
	lastSync     time.Time
	lastSyncOK   bool
	syncFailures int
}

func New(store *Store) *Server {
	return &Server{Store: store, Now: time.Now}
}

// SetConfig replaces the active fleet configuration.
func (s *Server) SetConfig(cfg *fleet.Config) {
	s.config.Store(cfg)
}

func (s *Server) Config() *fleet.Config {
	return s.config.Load()
}

// SyncResult records the outcome of a configuration sync for metrics.
func (s *Server) SyncResult(err error) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	s.lastSyncOK = err == nil
	if err == nil {
		s.lastSync = s.Now()
	} else {
		s.syncFailures++
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/config", s.handleConfig)
	mux.HandleFunc("POST /api/v1/report", s.handleReport)
	mux.HandleFunc("POST /api/v1/maintenance/lock", s.handleLock)
	mux.HandleFunc("DELETE /api/v1/maintenance/lock", s.handleUnlock)
	mux.HandleFunc("GET /api/v1/servers", s.handleServers)
	mux.HandleFunc("GET /api/v1/swarms", s.handleSwarms)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func lockGroup(d fleet.Desired) string {
	return d.Swarm + "/" + d.Environment
}

// desired resolves the configuration of a server and evaluates it against
// its last report. Swarm and environment fall back to the reported ones.
func (s *Server) desired(cfg *fleet.Config, hostname, swarm, environment string, records map[string]record, now time.Time) fleet.Desired {
	rec, ok := records[hostname]
	if ok {
		if swarm == "" {
			swarm = rec.Report.Swarm
		}
		if environment == "" {
			environment = rec.Report.Environment
		}
	}
	d := cfg.Resolve(hostname, swarm, environment, now)
	if ok {
		d.Evaluate(rec.Report.OSVersion, rec.Report.DockerVersion, rec.Report.BootTime, now)
	}
	return d
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if s.Config() == nil {
		http.Error(w, "configuration not loaded", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.Config()
	if cfg == nil {
		http.Error(w, "configuration not loaded", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	hostname := q.Get("hostname")
	if hostname == "" {
		http.Error(w, "hostname is required", http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, s.desired(cfg, hostname, q.Get("swarm"), q.Get("environment"), s.Store.Records(), s.Now()))
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	cfg := s.Config()
	if cfg == nil {
		http.Error(w, "configuration not loaded", http.StatusServiceUnavailable)
		return
	}
	var rep Report
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&rep); err != nil {
		http.Error(w, "invalid report: "+err.Error(), http.StatusBadRequest)
		return
	}
	if rep.Hostname == "" {
		http.Error(w, "hostname is required", http.StatusBadRequest)
		return
	}
	now := s.Now()
	if err := s.Store.Put(rep, remoteIP(r), now); err != nil {
		log.Printf("failed to persist reports: %v", err)
	}
	d := s.desired(cfg, rep.Hostname, rep.Swarm, rep.Environment, s.Store.Records(), now)
	if _, held := s.Store.Locks(now)[rep.Hostname]; held && !d.MaintenanceRequired {
		if err := s.canFinishMaintenance(cfg, d, rep.Hostname, now); err != nil {
			log.Printf("keeping maintenance lock of %s: %v", rep.Hostname, err)
		} else if s.Store.Release(rep.Hostname) {
			log.Printf("maintenance of %s finished, lock released", rep.Hostname)
		}
	}
	writeJSON(w, http.StatusOK, d)
}

type lockRequest struct {
	Hostname    string `json:"hostname"`
	Swarm       string `json:"swarm"`
	Environment string `json:"environment"`
	Reason      string `json:"reason"`
}

type lockResponse struct {
	Granted bool     `json:"granted"`
	Expires string   `json:"expires,omitempty"`
	Holders []string `json:"holders"`
	Message string   `json:"message,omitempty"`
}

func (s *Server) handleLock(w http.ResponseWriter, r *http.Request) {
	cfg := s.Config()
	if cfg == nil {
		http.Error(w, "configuration not loaded", http.StatusServiceUnavailable)
		return
	}
	var req lockRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil || req.Hostname == "" {
		http.Error(w, "hostname is required", http.StatusBadRequest)
		return
	}
	now := s.Now()
	d := s.desired(cfg, req.Hostname, req.Swarm, req.Environment, s.Store.Records(), now)
	if !d.MaintenanceWindowActive {
		writeJSON(w, http.StatusConflict, lockResponse{Holders: []string{}, Message: "outside of maintenance window"})
		return
	}
	check := func(locks map[string]Lock, records map[string]record) error {
		return s.canStartMaintenance(cfg, d, req.Hostname, locks, records, now)
	}
	expires, holders, err := s.Store.Acquire(req.Hostname, lockGroup(d), req.Reason, d.MaxConcurrentMaintenance, now, check)
	if err != nil {
		log.Printf("maintenance lock refused for %s: %v", req.Hostname, err)
		writeJSON(w, http.StatusConflict, lockResponse{Holders: holders, Message: err.Error()})
		return
	}
	log.Printf("maintenance lock (%s) granted to %s in %s", req.Reason, req.Hostname, lockGroup(d))
	writeJSON(w, http.StatusOK, lockResponse{Granted: true, Expires: expires.UTC().Format(time.RFC3339), Holders: holders})
}

func (s *Server) handleUnlock(w http.ResponseWriter, r *http.Request) {
	hostname := r.URL.Query().Get("hostname")
	if hostname == "" {
		http.Error(w, "hostname is required", http.StatusBadRequest)
		return
	}
	s.Store.Release(hostname)
	w.WriteHeader(http.StatusNoContent)
}

// ServerStatus is one entry of GET /api/v1/servers.
type ServerStatus struct {
	Hostname   string        `json:"hostname"`
	Known      bool          `json:"known"`
	Reported   *Report       `json:"reported"`
	LastSeen   *time.Time    `json:"last_seen"`
	RemoteAddr string        `json:"remote_addr,omitempty"`
	Desired    fleet.Desired `json:"desired"`
	UpToDate   bool          `json:"up_to_date"`
	Lock       *LockStatus   `json:"lock"`
}

type LockStatus struct {
	Reason  string    `json:"reason"`
	Expires time.Time `json:"expires"`
}

// statuses merges reports with servers listed in the configuration.
func (s *Server) statuses() []ServerStatus {
	cfg := s.Config()
	if cfg == nil {
		return nil
	}
	now := s.Now()
	records := s.Store.Records()
	locks := s.Store.Locks(now)

	names := map[string]bool{}
	for h := range records {
		names[h] = true
	}
	for h := range cfg.Servers {
		names[h] = true
	}
	for h := range locks {
		names[h] = true
	}

	out := make([]ServerStatus, 0, len(names))
	for h := range names {
		st := ServerStatus{Hostname: h, Desired: s.desired(cfg, h, "", "", records, now)}
		st.Known = st.Desired.Known
		if l, ok := locks[h]; ok {
			st.Lock = &LockStatus{Reason: l.Reason, Expires: l.Expires}
		}
		if rec, ok := records[h]; ok {
			rep, seen := rec.Report, rec.LastSeen
			st.Reported, st.LastSeen, st.RemoteAddr = &rep, &seen, rec.RemoteAddr
			st.UpToDate = !st.Desired.MaintenanceRequired
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hostname < out[j].Hostname })
	return out
}

func (s *Server) handleServers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.statuses())
}

// SwarmHealth is one entry of GET /api/v1/swarms.
type SwarmHealth struct {
	Swarm   string       `json:"swarm"`
	Healthy bool         `json:"healthy"`
	Message string       `json:"message,omitempty"`
	Servers []string     `json:"servers"`
	Status  *SwarmStatus `json:"status"`
}

func (s *Server) swarmHealth() []SwarmHealth {
	cfg := s.Config()
	if cfg == nil {
		return nil
	}
	now := s.Now()
	names := map[string]bool{}
	for _, st := range s.statuses() {
		if st.Desired.Swarm != "" {
			names[st.Desired.Swarm] = true
		}
	}
	ignore := map[string]bool{}
	for h := range s.Store.Locks(now) {
		ignore[h] = true
	}
	records := s.Store.Records()
	out := []SwarmHealth{}
	for name := range names {
		v, err := s.checkSwarm(cfg, name, ignore, records, now)
		h := SwarmHealth{Swarm: name, Healthy: err == nil, Status: v.manager, Servers: []string{}}
		if err != nil {
			h.Message = err.Error()
		}
		for _, m := range v.members {
			h.Servers = append(h.Servers, m.hostname)
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Swarm < out[j].Swarm })
	return out
}

func (s *Server) handleSwarms(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.swarmHealth())
}
