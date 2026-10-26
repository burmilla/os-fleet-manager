package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/burmilla/os-fleet-manager/internal/fleet"
	"golang.org/x/crypto/bcrypt"
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
        servers: {m1: {}, m2: {}, m3: {}, w1: {}}
`

type env struct {
	t   *testing.T
	s   *Server
	url string
	now time.Time
	// api is the API path the test servers use: their Swarm + Environment.
	api string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	cfg, err := fleet.Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Revision = "abc"
	e := &env{t: t, now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), api: "/api/v1/swarms/s1/prod"}
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
	code, body := e.post(e.api+"/report", Report{Hostname: host, OSVersion: osVersion, DockerVersion: "docker-29.9.2",
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
	code, body := e.post(e.api+"/maintenance/lock", lockRequest{Hostname: host, Reason: "os_upgrade"})
	var r lockResponse
	_ = json.Unmarshal(body, &r)
	return code == 200 && r.Granted, r.Message
}

func TestConfigEndpoint(t *testing.T) {
	e := newEnv(t)
	var d fleet.Desired
	_ = json.Unmarshal(e.get("/api/v1/swarms/s1/prod/config?hostname=m1"), &d)
	if d.Swarm != "s1" || d.Environment != "prod" || !d.Known || d.OSVersion != "v3.0.0" || d.ConfigRevision != "abc" || !d.MaintenanceWindowActive {
		t.Fatalf("%+v", d)
	}
	d = fleet.Desired{}
	_ = json.Unmarshal(e.get("/api/v1/config?hostname=m1"), &d)
	if d.Swarm != "" || d.Known {
		t.Fatalf("membership comes from the path only: %+v", d)
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
		`burmilla_fleet_swarm_unhealthy_reason{swarm="s1",reason="`,
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
	e.api = "/api/v1"
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

func TestLocksPersist(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	s := NewStore(dir, time.Hour)
	if _, _, err := s.Acquire("a", "s1/prod", "os_upgrade", 1, now, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Acquire("b", "s1/test", "restart", 1, now, nil); err != nil {
		t.Fatal(err)
	}
	s.Release("b")

	// A restarted service keeps the lock and still refuses the slot.
	s = NewStore(dir, time.Hour)
	locks := s.Locks(now)
	if len(locks) != 1 || locks["a"].Group != "s1/prod" || locks["a"].Reason != "os_upgrade" {
		t.Fatalf("locks not persisted: %+v", locks)
	}
	if _, _, err := s.Acquire("c", "s1/prod", "restart", 1, now, nil); err == nil {
		t.Fatal("persisted lock must still occupy the slot")
	}
	if len(s.Locks(now.Add(2*time.Hour))) != 0 {
		t.Fatal("persisted lock should still expire")
	}
}

func TestLockNotGrantedWhenNotPersisted(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir, time.Hour)
	if err := os.Mkdir(filepath.Join(dir, "locks.json.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Acquire("a", "g", "restart", 1, time.Now(), nil); err == nil {
		t.Fatal("lock must be refused when it cannot be written")
	}
	if len(s.Locks(time.Now())) != 0 {
		t.Fatal("refused lock must not stay in memory")
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

func TestAuthentication(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	cfg, err := fleet.Parse([]byte(`
defaults: {os_version: v3.0.0}
swarms:
  s1:
    environments:
      prod: {htpasswd: ["s1-prod:` + string(hash) + `"]}
      test: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	e.s.SetConfig(cfg)
	const prod, test = "/api/v1/swarms/s1/prod", "/api/v1/swarms/s1/test"

	do := func(method, path, user, pass string, rep any) (int, string) {
		var body io.Reader
		if rep != nil {
			data, _ := json.Marshal(rep)
			body = bytes.NewReader(data)
		}
		req, _ := http.NewRequest(method, e.url+path, body)
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}

	if code, _ := do("POST", prod+"/report", "", "", Report{Hostname: "a"}); code != http.StatusUnauthorized {
		t.Fatalf("protected environment without credentials: %d", code)
	}
	if code, _ := do("POST", prod+"/report", "s1-prod", "wrong", Report{Hostname: "a"}); code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", code)
	}
	if code, body := do("POST", test+"/report", "s1-prod", "secret", Report{Hostname: "a"}); code != http.StatusForbidden || !strings.Contains(body, "credentials are for s1/prod") {
		t.Fatalf("credentials of another environment: %d %s", code, body)
	}
	// The path decides the Swarm + Environment, whatever the body says.
	code, body := do("POST", prod+"/report", "s1-prod", "secret", Report{Hostname: "a", Swarm: "s1", Environment: "test"})
	if code != 200 || !strings.Contains(body, `"environment":"prod"`) {
		t.Fatalf("valid credentials: %d %s", code, body)
	}
	if got := e.s.Store.Records()["a"].Report.Environment; got != "prod" {
		t.Fatalf("report stored in %s", got)
	}
	// A host reporting to prod cannot be taken over from elsewhere...
	for _, path := range []string{test, "/api/v1"} {
		if code, body := do("POST", path+"/report", "", "", Report{Hostname: "a"}); code != http.StatusForbidden || !strings.Contains(body, "a reports to s1/prod") {
			t.Fatalf("takeover from %s: %d %s", path, code, body)
		}
	}
	if code, _ := do("POST", test+"/report", "", "", Report{Hostname: "b"}); code != 200 {
		t.Fatalf("unprotected environment: %d", code)
	}
	// ...until it has stopped reporting there.
	e.now = e.now.Add(time.Hour)
	if code, body := do("POST", test+"/report", "", "", Report{Hostname: "a"}); code != 200 {
		t.Fatalf("stale host may move: %d %s", code, body)
	}
	do("POST", prod+"/report", "s1-prod", "secret", Report{Hostname: "a"})

	if code, _ := do("DELETE", prod+"/maintenance/lock?hostname=a", "", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("unlock without credentials: %d", code)
	}
	resp, _ := http.Get(e.url + prod + "/config?hostname=a")
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("config without credentials: %d", resp.StatusCode)
	}

	if _, err := fleet.Parse([]byte(`
swarms:
  s1:
    environments:
      a: {htpasswd: ["u:` + string(hash) + `"]}
      b: {htpasswd: ["u:` + string(hash) + `"]}
`)); err == nil {
		t.Fatal("duplicate htpasswd user must be rejected")
	}
}

func TestSSHStatus(t *testing.T) {
	e := newEnv(t)
	e.api = "/api/v1"
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOiuTuZpapVCjAUUGcXj2zMLBbrr0QyMsca1UEx+EBWQ alice@example.com"
	cfg, err := fleet.Parse([]byte("defaults: {ssh: {authorized_keys: [\"" + key + "\"], password_authentication: false}}"))
	if err != nil {
		t.Fatal(err)
	}
	e.s.SetConfig(cfg)

	var d fleet.Desired
	_ = json.Unmarshal(e.get("/api/v1/config?hostname=solo"), &d)
	if len(d.SSH.AuthorizedKeys) != 1 || d.SSH.PasswordAuthentication == nil || *d.SSH.PasswordAuthentication {
		t.Fatalf("unexpected ssh config: %+v", d.SSH)
	}

	on := true
	_, body := e.post("/api/v1/report", Report{Hostname: "solo", SSH: &fleet.SSH{AuthorizedKeys: []string{key}, PasswordAuthentication: &on}})
	_ = json.Unmarshal(body, &d)
	if d.SSHInSync {
		t.Fatal("password authentication is still on")
	}
	metrics := string(e.get("/metrics"))
	for _, want := range []string{
		`burmilla_fleet_server_ssh_in_sync{hostname="solo",swarm="",environment=""} 0`,
		`burmilla_fleet_server_ssh_password_authentication{hostname="solo",swarm="",environment=""} 1`,
	} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("missing %s in\n%s", want, metrics)
		}
	}

	off := false
	_, body = e.post("/api/v1/report", Report{Hostname: "solo", SSH: &fleet.SSH{AuthorizedKeys: []string{key}, PasswordAuthentication: &off}})
	_ = json.Unmarshal(body, &d)
	if !d.SSHInSync {
		t.Fatalf("should be in sync: %s", body)
	}
}

func TestLocalContainers(t *testing.T) {
	e := newEnv(t)
	e.api = "/api/v1"
	cfg, _ := fleet.Parse([]byte("defaults: {max_uptime: 30d}"))
	e.s.SetConfig(cfg)
	boot := e.now.Add(-40 * 24 * time.Hour)
	starting := &ContainerStatus{Containers: 2, ContainersReady: 1, ContainersNotReady: []ContainerState{
		{Name: "db", State: "running", Health: "starting", RestartPolicy: "always"}}}
	ready := &ContainerStatus{Containers: 2, ContainersReady: 2}

	e.post("/api/v1/report", Report{Hostname: "solo", BootTime: boot, Containers: starting})
	if ok, msg := e.lock("solo"); ok || msg != "container db on solo is starting" {
		t.Fatalf("lock must wait for local containers: %v %s", ok, msg)
	}
	e.post("/api/v1/report", Report{Hostname: "solo", BootTime: boot, Containers: ready})
	if ok, msg := e.lock("solo"); !ok {
		t.Fatalf("lock expected: %s", msg)
	}

	// Rebooted, but the containers are not back yet: the lock is kept.
	e.post("/api/v1/report", Report{Hostname: "solo", BootTime: e.now, Containers: starting})
	if _, held := e.s.Store.Locks(e.now)["solo"]; !held {
		t.Fatal("lock must be kept until local containers are ready")
	}
	e.post("/api/v1/report", Report{Hostname: "solo", BootTime: e.now, Containers: ready})
	if _, held := e.s.Store.Locks(e.now)["solo"]; held {
		t.Fatal("lock should be released")
	}
	metrics := string(e.get("/metrics"))
	if want := `burmilla_fleet_server_containers{hostname="solo",swarm="",environment="",state="ready"} 2`; !strings.Contains(metrics, want) {
		t.Fatalf("missing %s", want)
	}
}

func TestLocalContainersInSwarm(t *testing.T) {
	e := newEnv(t)
	for _, h := range []string{"m1", "m2", "m3"} {
		e.report(h, "v2.9.0", 4, 3)
	}
	st := &SwarmStatus{NodeID: "w1", ClusterID: "c1", LocalNodeState: "active"}
	e.post(e.api+"/report", Report{Hostname: "w1", OSVersion: "v3.0.0", DockerVersion: "docker-29.9.2", BootTime: e.now.Add(-time.Hour),
		State: "idle", SwarmStatus: st, Containers: &ContainerStatus{Containers: 1, ContainersNotReady: []ContainerState{
			{Name: "agent", State: "exited", ExitCode: 137, RestartPolicy: "unless-stopped"}}}})
	if ok, msg := e.lock("m1"); ok || msg != "container agent on w1 exited with code 137" {
		t.Fatalf("a broken container on another node must block: %v %s", ok, msg)
	}
}

func TestStandaloneGroup(t *testing.T) {
	e := newEnv(t)
	cfg, err := fleet.Parse([]byte(`
defaults: {max_uptime: 30d}
swarms:
  edge:
    standalone: true
    environments:
      prod: {max_concurrent_maintenance: 1}
`))
	if err != nil {
		t.Fatal(err)
	}
	e.s.SetConfig(cfg)
	e.api = "/api/v1/swarms/edge/prod"
	boot := e.now.Add(-40 * 24 * time.Hour)
	broken := &ContainerStatus{Containers: 1, ContainersNotReady: []ContainerState{{Name: "app", State: "restarting"}}}
	// No Swarm status at all: standalone servers are not Swarm members.
	e.post(e.api+"/report", Report{Hostname: "e1", BootTime: boot})
	_, body := e.post(e.api+"/report", Report{Hostname: "e2", BootTime: boot, Containers: broken})
	var d fleet.Desired
	_ = json.Unmarshal(body, &d)
	if !d.Standalone {
		t.Fatalf("standalone expected: %s", body)
	}

	if ok, msg := e.lock("e1"); !ok {
		t.Fatalf("other servers of a standalone group do not block: %s", msg)
	}
	if ok, msg := e.lock("e3"); ok || msg != "1 of 1 maintenance slots in use" {
		t.Fatalf("max_concurrent_maintenance still applies: %v %s", ok, msg)
	}
	e.s.Store.Release("e1")
	if ok, msg := e.lock("e2"); ok || msg != "container app on e2 is restarting" {
		t.Fatalf("own containers block: %v %s", ok, msg)
	}
	if strings.Contains(string(e.get("/api/v1/swarms")), "edge") {
		t.Fatal("standalone groups are not Swarms")
	}
}

func TestNomadQEMUNodes(t *testing.T) {
	e := newEnv(t)
	cfg, err := fleet.Parse([]byte(`
defaults:
  os_version: v3.0.0
  docker_version: docker-29.9.2
  upgrade_policy: auto
swarms:
  nomad:
    os_version: v20261024-1
    environments:
      prod: {max_concurrent_maintenance: 2}
`))
	if err != nil {
		t.Fatal(err)
	}
	e.s.SetConfig(cfg)
	e.api = "/api/v1/swarms/nomad/prod"
	ready := func() *NomadStatus {
		return &NomadStatus{NodeID: "id", Status: "ready", Eligibility: "eligible", Allocations: 3}
	}
	report := func(host, osVersion string, n *NomadStatus) fleet.Desired {
		t.Helper()
		code, body := e.post(e.api+"/report", Report{Hostname: host, NodeType: fleet.NodeTypeNomadQEMU, OSVersion: osVersion,
			QEMUVersion: "10.1.0", NomadVersion: "2.0.7", BootTime: e.now.Add(-time.Hour), State: "idle", Nomad: n})
		if code != 200 {
			t.Fatalf("report %s: %d %s", host, code, body)
		}
		var d fleet.Desired
		_ = json.Unmarshal(body, &d)
		return d
	}

	d := report("n1", "v20260324-1", ready())
	if d.OSImage != fleet.DefaultNomadQEMUImage || d.OSVersion != "v20261024-1" || d.DockerVersion != "" || !d.Standalone ||
		len(d.MaintenanceReasons) != 1 || d.MaintenanceReasons[0] != fleet.ReasonOSUpgrade {
		t.Fatalf("Nomad QEMU nodes get no BurmillaOS or Docker settings: %+v", d)
	}
	if d := report("n2", "v20261024-1", ready()); d.MaintenanceRequired {
		t.Fatalf("up to date node: %+v", d)
	}

	// A drained node elsewhere in the group blocks the drain of another.
	drained := ready()
	drained.Eligibility, drained.Drain = "ineligible", true
	report("n2", "v20261024-1", drained)
	if ok, msg := e.lock("n1"); ok || msg != "Nomad node n2 is draining" {
		t.Fatalf("draining peer must block: %v %s", ok, msg)
	}
	report("n2", "v20261024-1", ready())
	if ok, msg := e.lock("n1"); !ok {
		t.Fatalf("healthy group should grant the lock: %s", msg)
	}

	// Back on the target version, but still ineligible: the lock stays.
	n := ready()
	n.Eligibility = "ineligible"
	report("n1", "v20261024-1", n)
	if _, held := e.s.Store.Locks(e.now)["n1"]; !held {
		t.Fatal("lock released before the node is eligible again")
	}
	report("n1", "v20261024-1", ready())
	if _, held := e.s.Store.Locks(e.now)["n1"]; held {
		t.Fatal("lock not released after the node is back")
	}

	// A peer which stopped reporting blocks too.
	report("n1", "v20260324-1", ready())
	e.now = e.now.Add(20 * time.Minute)
	report("n1", "v20260324-1", ready())
	if ok, msg := e.lock("n1"); ok || !strings.Contains(msg, "n2 has not reported") {
		t.Fatalf("stale peer must block: %v %s", ok, msg)
	}

	metrics := string(e.get("/metrics"))
	for _, want := range []string{
		`burmilla_fleet_server_info{hostname="n1",swarm="nomad",environment="prod",node_type="nomad-qemu-node",os_version="v20260324-1",docker_version="",kernel_version="",client_version="",qemu_version="10.1.0",nomad_version="2.0.7"} 1`,
		`burmilla_fleet_server_nomad_node{hostname="n1",swarm="nomad",environment="prod",node_type="nomad-qemu-node",status="ready",eligibility="eligible",drain="false"} 1`,
		`burmilla_fleet_server_nomad_allocations{hostname="n1",swarm="nomad",environment="prod",node_type="nomad-qemu-node"} 3`,
		`burmilla_fleet_server_desired_info{hostname="n1",swarm="nomad",environment="prod",node_type="nomad-qemu-node",os_version="v20261024-1",docker_version="",upgrade_policy="auto"} 1`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(metrics, "nomad-qemu-node\",os_version=\"v3.0.0\"") {
		t.Error("BurmillaOS target version leaked to a Nomad QEMU node")
	}
}

func TestRAIDStatus(t *testing.T) {
	e := newEnv(t)
	cfg, _ := fleet.Parse([]byte("defaults: {}"))
	e.s.SetConfig(cfg)
	raid := []RAIDArray{
		{Name: "md0", Level: "raid1", State: "active", Devices: 2, ActiveDevices: 2},
		{Name: "md1", Level: "raid1", State: "active", Devices: 2, ActiveDevices: 1, Degraded: true,
			FailedDevices: []string{"sdb3"}, SpareDevices: []string{"sdc1"}, SyncAction: "recovery", SyncProgress: 12.6},
	}
	if code, body := e.post("/api/v1/report", Report{Hostname: "n1", NodeType: fleet.NodeTypeNomadQEMU, RAID: raid}); code != 200 {
		t.Fatalf("report: %d %s", code, body)
	}
	metrics := string(e.get("/metrics"))
	base := `hostname="n1",swarm="",environment="",node_type="nomad-qemu-node",array=`
	for _, want := range []string{
		`burmilla_fleet_server_raid_array{` + base + `"md0",level="raid1",state="active"} 1`,
		`burmilla_fleet_server_raid_degraded{` + base + `"md0"} 0`,
		`burmilla_fleet_server_raid_degraded{` + base + `"md1"} 1`,
		`burmilla_fleet_server_raid_devices{` + base + `"md1",state="active"} 1`,
		`burmilla_fleet_server_raid_devices{` + base + `"md1",state="missing"} 1`,
		`burmilla_fleet_server_raid_devices{` + base + `"md1",state="failed"} 1`,
		`burmilla_fleet_server_raid_devices{` + base + `"md1",state="spare"} 1`,
		`burmilla_fleet_server_raid_sync_progress_percent{` + base + `"md1",action="recovery"} 12.6`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(metrics, `raid_sync_progress_percent{`+base+`"md0"`) {
		t.Error("no sync progress for an idle array")
	}
	if !strings.Contains(string(e.get("/api/v1/servers")), `"failed_devices":["sdb3"]`) {
		t.Error("RAID status missing from /api/v1/servers")
	}
}
