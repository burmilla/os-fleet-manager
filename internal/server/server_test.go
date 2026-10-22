package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/burmilla/os-fleet-manager/internal/fleet"
)

const testConfig = `
defaults:
  os_version: v3.0.0
  docker_version: docker-29.9.2
  upgrade_policy: auto
swarms:
  s1:
    environments:
      prod:
        max_concurrent_maintenance: 2
servers:
  m1: {swarm: s1, environment: prod}
  m2: {swarm: s1, environment: prod}
  m3: {swarm: s1, environment: prod}
  w1: {swarm: s1, environment: prod}
`

type env struct {
	t   *testing.T
	s   *Server
	url string
	now time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	cfg, err := fleet.Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Revision = "abc"
	e := &env{t: t, now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	e.s = New(NewStore(t.TempDir(), time.Hour))
	e.s.Now = func() time.Time { return e.now }
	e.s.SetConfig(cfg)
	e.s.SyncResult(nil)
	ts := httptest.NewServer(e.s.Handler())
	t.Cleanup(ts.Close)
	e.url = ts.URL
	return e
}

func (e *env) post(path string, body any) (int, []byte) {
	e.t.Helper()
	data, _ := json.Marshal(body)
	resp, err := http.Post(e.url+path, "application/json", bytes.NewReader(data))
	if err != nil {
		e.t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (e *env) get(path string) []byte {
	e.t.Helper()
	resp, err := http.Get(e.url + path)
	if err != nil {
		e.t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	return out
}

// report sends a healthy report. Managers see ready nodes out of 4 and
// reachable managers out of 3.
func (e *env) report(host, osVersion string, ready, reachable int, notReady ...ServiceState) fleet.Desired {
	e.t.Helper()
	st := &SwarmStatus{NodeID: host, ClusterID: "c1", LocalNodeState: "active"}
	if strings.HasPrefix(host, "m") {
		st.Manager, st.Nodes, st.NodesReady, st.Managers, st.ManagersReachable = true, 4, ready, 3, reachable
		st.Services, st.ServicesReady, st.ServicesNotReady = 5, 5-len(notReady), notReady
	}
	code, body := e.post("/api/v1/report", Report{Hostname: host, OSVersion: osVersion, DockerVersion: "docker-29.9.2",
		BootTime: e.now.Add(-time.Hour), State: "idle", SwarmStatus: st})
	if code != 200 {
		e.t.Fatalf("report %s: %d %s", host, code, body)
	}
	var d fleet.Desired
	_ = json.Unmarshal(body, &d)
	return d
}

func (e *env) lock(host string) (bool, string) {
	e.t.Helper()
	code, body := e.post("/api/v1/maintenance/lock", lockRequest{Hostname: host, Reason: "os_upgrade"})
	var r lockResponse
	_ = json.Unmarshal(body, &r)
	return code == 200 && r.Granted, r.Message
}

func TestConfigEndpoint(t *testing.T) {
	e := newEnv(t)
	var d fleet.Desired
	_ = json.Unmarshal(e.get("/api/v1/config?hostname=m1"), &d)
	if d.Swarm != "s1" || d.OSVersion != "v3.0.0" || d.ConfigRevision != "abc" || !d.MaintenanceWindowActive {
		t.Fatalf("%+v", d)
	}
	resp, _ := http.Get(e.url + "/api/v1/config")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestMaintenanceLockFollowsSwarmHealth(t *testing.T) {
	e := newEnv(t)

	// Nobody has reported yet, so the Swarm state is unknown.
	if ok, msg := e.lock("w1"); ok || !strings.Contains(msg, "never reported") {
		t.Fatalf("lock must be refused before the Swarm is known: %s", msg)
	}

	for _, h := range []string{"m1", "m2", "m3", "w1"} {
		if d := e.report(h, "v2.0.3", 4, 3); !d.MaintenanceRequired || d.MaintenanceReasons[0] != fleet.ReasonOSUpgrade {
			t.Fatalf("os upgrade expected: %+v", d)
		}
	}
	if ok, msg := e.lock("m1"); !ok {
		t.Fatalf("healthy Swarm should grant the lock: %s", msg)
	}
	// Two slots are configured but a second manager would break quorum.
	if ok, msg := e.lock("m2"); ok || !strings.Contains(msg, "quorum") {
		t.Fatalf("second manager must be refused: %s", msg)
	}
	// m1 reboots: m2 sees it down. The worker can use the second slot
	// because the only node down is the lock holder.
	e.now = e.now.Add(time.Minute)
	e.report("m2", "v2.0.3", 3, 2)
	if ok, msg := e.lock("w1"); !ok {
		t.Fatalf("worker should get the second slot: %s", msg)
	}
	e.s.Store.Release("w1")

	// Global services may miss the task of the server in maintenance,
	// replicated services must be rescheduled first.
	e.report("m2", "v2.0.3", 3, 2, ServiceState{Name: "agent", Mode: "global", DesiredTasks: 4, RunningTasks: 3})
	if ok, msg := e.lock("w1"); !ok {
		t.Fatalf("missing global task of the lock holder is fine: %s", msg)
	}
	e.s.Store.Release("w1")
	e.report("m2", "v2.0.3", 3, 2, ServiceState{Name: "web", Mode: "replicated", DesiredTasks: 3, RunningTasks: 2})
	if ok, msg := e.lock("w1"); ok || msg != "service web runs 2 of 3 tasks (Swarm s1, seen by m2)" {
		t.Fatalf("missing replica must block: %s", msg)
	}
	e.report("m2", "v2.0.3", 3, 2)

	// m1 is upgraded but the Swarm still sees it down: keep the lock.
	e.now = e.now.Add(time.Minute)
	if d := e.report("m1", "v3.0.0", 3, 2); d.MaintenanceRequired {
		t.Fatalf("m1 should be up to date: %+v", d)
	}
	if _, held := e.s.Store.Locks(e.now)["m1"]; !held {
		t.Fatal("lock must be kept until the Swarm is healthy")
	}
	// All nodes are back but a service update is still rolling out.
	updating := ServiceState{Name: "api", Mode: "replicated", DesiredTasks: 2, RunningTasks: 2, UpdateState: "updating"}
	e.report("m1", "v3.0.0", 4, 3, updating)
	if _, held := e.s.Store.Locks(e.now)["m1"]; !held {
		t.Fatal("lock must be kept while a service is not at its target configuration")
	}
	// Once a manager sees everything ready, the next report releases it.
	e.report("m2", "v2.0.3", 4, 3)
	e.report("m1", "v3.0.0", 4, 3)
	if _, held := e.s.Store.Locks(e.now)["m1"]; held {
		t.Fatal("lock should be released")
	}

	// A server that stops reporting blocks maintenance of others.
	e.now = e.now.Add(30 * time.Minute)
	for _, h := range []string{"m1", "m2", "m3"} {
		e.report(h, "v3.0.0", 4, 3)
	}
	if ok, msg := e.lock("m2"); ok || !strings.Contains(msg, "w1 has not reported") {
		t.Fatalf("stale worker must block: %s", msg)
	}

	body := string(e.get("/metrics"))
	for _, want := range []string{
		`burmilla_fleet_server_up_to_date{hostname="m1",swarm="s1",environment="prod"} 1`,
		`burmilla_fleet_server_up_to_date{hostname="w1",swarm="s1",environment="prod"} 0`,
		`burmilla_fleet_server_maintenance_required{hostname="w1",swarm="s1",environment="prod",reason="os_upgrade"} 1`,
		`burmilla_fleet_swarm_healthy{swarm="s1"} 0`,
		`burmilla_fleet_swarm_nodes{swarm="s1",state="ready"} 4`,
		`burmilla_fleet_swarm_services{swarm="s1",state="ready"} 5`,
		`burmilla_fleet_config_info{revision="abc"} 1`,
		`burmilla_fleet_config_sync_success 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %s\n%s", want, body)
		}
	}

	var swarms []SwarmHealth
	_ = json.Unmarshal(e.get("/api/v1/swarms"), &swarms)
	if len(swarms) != 1 || swarms[0].Healthy || len(swarms[0].Servers) != 4 {
		t.Fatalf("unexpected swarms: %+v", swarms)
	}
}

func TestStandaloneServerAndScheduledRestart(t *testing.T) {
	e := newEnv(t)
	cfg, _ := fleet.Parse([]byte("defaults: {max_uptime: 30d, maintenance_window: {start: '02:00', duration: 2h}}"))
	e.s.SetConfig(cfg)
	e.now = time.Date(2026, 10, 22, 2, 30, 0, 0, time.UTC)

	code, body := e.post("/api/v1/report", Report{Hostname: "solo", BootTime: e.now.Add(-40 * 24 * time.Hour)})
	var d fleet.Desired
	_ = json.Unmarshal(body, &d)
	if code != 200 || !d.RestartRequired || d.MaintenanceReasons[0] != fleet.ReasonRestart {
		t.Fatalf("restart expected: %s", body)
	}
	if ok, msg := e.lock("solo"); !ok {
		t.Fatalf("server outside Swarm only needs a slot: %s", msg)
	}
	e.post("/api/v1/report", Report{Hostname: "solo", BootTime: e.now})
	if _, held := e.s.Store.Locks(e.now)["solo"]; held {
		t.Fatal("lock should be released after restart")
	}
	e.now = e.now.Add(3 * time.Hour)
	if ok, msg := e.lock("solo"); ok || msg != "outside of maintenance window" {
		t.Fatalf("lock outside window must be refused: %s", msg)
	}

	req, _ := http.NewRequest(http.MethodDelete, e.url+"/api/v1/maintenance/lock?hostname=solo", nil)
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != http.StatusNoContent {
		t.Fatalf("unlock failed: %d", r.StatusCode)
	}
}

func TestReportsPersist(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir, time.Minute)
	if err := s.Put(Report{Hostname: "a", OSVersion: "v3.0.0"}, "10.0.0.1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := NewStore(dir, time.Minute).Records()["a"]; got.Report.OSVersion != "v3.0.0" || got.RemoteAddr != "10.0.0.1" {
		t.Fatalf("report not persisted: %+v", got)
	}
}

func TestLockExpires(t *testing.T) {
	s := NewStore("", time.Minute)
	now := time.Now()
	if _, _, err := s.Acquire("a", "g", "restart", 1, now, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Acquire("b", "g", "restart", 1, now.Add(30*time.Second), nil); err == nil {
		t.Fatal("lock should still be held")
	}
	if _, _, err := s.Acquire("b", "g", "restart", 1, now.Add(2*time.Minute), nil); err != nil {
		t.Fatal("lock should have expired")
	}
}
