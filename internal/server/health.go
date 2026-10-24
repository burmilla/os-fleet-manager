package server

import (
	"fmt"
	"sort"
	"time"

	"github.com/burmilla/os-fleet-manager/internal/fleet"
)

// staleAfter is how long a report is trusted. Clients report every check
// interval, so missing three reports in a row means the server is down.
func staleAfter(d fleet.Desired) time.Duration {
	return 3*time.Duration(d.CheckIntervalSeconds)*time.Second + time.Minute
}

type member struct {
	hostname string
	rec      *record
	desired  fleet.Desired
}

// swarmMembers returns every server which reports to the Swarm, and the
// servers listed for it in the configuration which have never reported.
func (s *Server) swarmMembers(cfg *fleet.Config, swarm string, records map[string]record, now time.Time) []member {
	var out []member
	for h, rec := range records {
		if rec.Report.Swarm == swarm {
			rec := rec
			out = append(out, member{h, &rec, s.desired(cfg, h, swarm, rec.Report.Environment, records, now)})
		}
	}
	for h, where := range cfg.Listed() {
		if _, reported := records[h]; !reported && where[0] == swarm {
			out = append(out, member{h, nil, cfg.Resolve(h, swarm, where[1], now)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].hostname < out[j].hostname })
	return out
}

// swarmView is the Swarm wide state, taken from the freshest manager report.
type swarmView struct {
	members        []member
	manager        *SwarmStatus
	managerHost    string
	ignored        int
	ignoredManager int
}

// checkSwarm verifies that the whole Swarm is healthy. Servers in ignore
// hold a maintenance lock and are allowed to be down.
func (s *Server) checkSwarm(cfg *fleet.Config, swarm string, ignore map[string]bool, records map[string]record, now time.Time) (*swarmView, error) {
	v := &swarmView{members: s.swarmMembers(cfg, swarm, records, now)}
	clusterID := ""
	var managerSeen time.Time
	for _, m := range v.members {
		if ignore[m.hostname] {
			v.ignored++
			if m.rec == nil || m.rec.Report.SwarmStatus == nil || m.rec.Report.SwarmStatus.Manager {
				v.ignoredManager++
			}
			continue
		}
		if m.rec == nil {
			return v, fmt.Errorf("%s has never reported", m.hostname)
		}
		rep := m.rec.Report
		if now.Sub(m.rec.LastSeen) > staleAfter(m.desired) {
			return v, fmt.Errorf("%s has not reported since %s", m.hostname, m.rec.LastSeen.UTC().Format(time.RFC3339))
		}
		if rep.State == "error" {
			return v, fmt.Errorf("%s reports error: %s", m.hostname, rep.Message)
		}
		if err := containerProblem(m.hostname, rep.Containers); err != nil {
			return v, err
		}
		st := rep.SwarmStatus
		if st == nil || st.LocalNodeState != "active" {
			state := "unknown"
			if st != nil {
				state = st.LocalNodeState
			}
			return v, fmt.Errorf("%s is not active in the Swarm (state %s)", m.hostname, state)
		}
		if clusterID == "" {
			clusterID = st.ClusterID
		} else if st.ClusterID != "" && st.ClusterID != clusterID {
			return v, fmt.Errorf("%s is in another Swarm cluster (%s) than the other servers (%s)", m.hostname, st.ClusterID, clusterID)
		}
		if st.Manager && st.Nodes > 0 && m.rec.LastSeen.After(managerSeen) {
			v.manager, v.managerHost, managerSeen = st, m.hostname, m.rec.LastSeen
		}
	}
	if len(v.members) == 0 {
		return v, nil
	}
	if v.manager == nil {
		return v, fmt.Errorf("no manager of Swarm %s has reported the Swarm status", swarm)
	}
	if down := v.manager.Nodes - v.manager.NodesReady; down > v.ignored {
		return v, fmt.Errorf("%d nodes of Swarm %s are not ready (seen by %s)", down, swarm, v.managerHost)
	}
	if down := v.manager.Managers - v.manager.ManagersReachable; down > v.ignoredManager {
		return v, fmt.Errorf("%d managers of Swarm %s are unreachable (seen by %s)", down, swarm, v.managerHost)
	}
	for _, svc := range v.manager.ServicesNotReady {
		if err := serviceProblem(svc, v.ignored); err != nil {
			return v, fmt.Errorf("%w (Swarm %s, seen by %s)", err, swarm, v.managerHost)
		}
	}
	return v, nil
}

// serviceProblem tells why a service blocks maintenance. Global services
// can miss one task per server in maintenance; replicated services must run
// all replicas, so their tasks have been rescheduled to the other nodes.
func serviceProblem(svc ServiceState, inMaintenance int) error {
	switch svc.UpdateState {
	case "updating", "paused", "rollback_started", "rollback_paused":
		return fmt.Errorf("service %s is not at its target configuration (update %s)", svc.Name, svc.UpdateState)
	}
	missing := svc.DesiredTasks - svc.RunningTasks
	if missing <= 0 || (svc.Mode == "global" && missing <= inMaintenance) {
		return nil
	}
	return fmt.Errorf("service %s runs %d of %d tasks", svc.Name, svc.RunningTasks, svc.DesiredTasks)
}

// containerProblem tells why the local containers of a server block
// maintenance. Reports without container status (older clients, User
// Docker not reachable) do not block.
func containerProblem(hostname string, cs *ContainerStatus) error {
	if cs == nil || len(cs.ContainersNotReady) == 0 {
		return nil
	}
	c := cs.ContainersNotReady[0]
	state := "is " + c.State
	if c.State == "running" && c.Health != "" {
		state = "is " + c.Health
	} else if c.State == "exited" {
		state = fmt.Sprintf("exited with code %d", c.ExitCode)
	}
	msg := fmt.Sprintf("container %s on %s %s", c.Name, hostname, state)
	if n := len(cs.ContainersNotReady); n > 1 {
		msg += fmt.Sprintf(" (%d containers not ready)", n)
	}
	return fmt.Errorf("%s", msg)
}

// nomadProblem tells why the Nomad node of a Nomad QEMU node blocks
// maintenance: it must be ready, eligible for scheduling and not
// draining. Reports without Nomad status (no ACL token, BurmillaOS) do not
// block.
func nomadProblem(hostname string, st *NomadStatus) error {
	switch {
	case st == nil:
		return nil
	case st.Status != "ready":
		return fmt.Errorf("Nomad node %s is %s", hostname, st.Status)
	case st.Drain:
		return fmt.Errorf("Nomad node %s is draining", hostname)
	case st.Eligibility != "eligible":
		return fmt.Errorf("Nomad node %s is not eligible for scheduling", hostname)
	}
	return nil
}

// nomadGroupProblem checks the other Nomad QEMU nodes of the group of a
// Nomad QEMU node before it is drained: every one which does not hold a
// maintenance lock must report, not be in error and have a Nomad node
// which is ready and eligible, so that the VMs have somewhere to go.
func (s *Server) nomadGroupProblem(cfg *fleet.Config, d fleet.Desired, hostname string, locks map[string]Lock, records map[string]record, now time.Time) error {
	if d.NodeType != fleet.NodeTypeNomadQEMU || d.Swarm == "" {
		return nil
	}
	hosts := make([]string, 0, len(records))
	for h := range records {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		rec := records[h]
		rep := rec.Report
		if _, held := locks[h]; held || h == hostname || rep.Swarm != d.Swarm || rep.NodeType != fleet.NodeTypeNomadQEMU {
			continue
		}
		md := s.desired(cfg, h, rep.Swarm, rep.Environment, records, now)
		if now.Sub(rec.LastSeen) > staleAfter(md) {
			return fmt.Errorf("%s has not reported since %s", h, rec.LastSeen.UTC().Format(time.RFC3339))
		}
		if rep.State == "error" {
			return fmt.Errorf("%s reports error: %s", h, rep.Message)
		}
		if err := nomadProblem(h, rep.Nomad); err != nil {
			return err
		}
	}
	return nil
}

// localProblem checks a server outside of a Swarm: its own last report.
func localProblem(hostname string, records map[string]record) error {
	rec, ok := records[hostname]
	if !ok {
		return nil
	}
	return containerProblem(hostname, rec.Report.Containers)
}

// canStartMaintenance decides if hostname may take a maintenance lock.
func (s *Server) canStartMaintenance(cfg *fleet.Config, d fleet.Desired, hostname string, locks map[string]Lock, records map[string]record, now time.Time) error {
	if d.Swarm == "" || d.Standalone {
		if err := localProblem(hostname, records); err != nil {
			return err
		}
		return s.nomadGroupProblem(cfg, d, hostname, locks, records, now)
	}
	ignore := map[string]bool{}
	for h := range locks {
		if h != hostname {
			ignore[h] = true
		}
	}
	v, err := s.checkSwarm(cfg, d.Swarm, ignore, records, now)
	if err != nil {
		return err
	}
	rec, ok := records[hostname]
	if !ok || rec.Report.SwarmStatus == nil {
		return fmt.Errorf("%s has not reported its Swarm status", hostname)
	}
	if rec.Report.SwarmStatus.Manager && v.manager != nil {
		managers := v.manager.Managers
		unavailable := max(managers-v.manager.ManagersReachable, v.ignoredManager) + 1
		if managers >= 3 && managers-unavailable <= managers/2 {
			return fmt.Errorf("taking %s down would lose the quorum of Swarm %s (%d managers, %d unavailable)", hostname, d.Swarm, managers, unavailable)
		}
		if managers < 3 && unavailable > 1 {
			return fmt.Errorf("another manager of Swarm %s is unavailable", d.Swarm)
		}
	}
	return nil
}

// canFinishMaintenance decides if the lock of hostname can be released:
// the server must be back in the Swarm and the whole Swarm healthy, and
// its local containers ready. A Nomad QEMU node must be back in Nomad.
func (s *Server) canFinishMaintenance(cfg *fleet.Config, d fleet.Desired, hostname string, now time.Time) error {
	if d.Swarm == "" || d.Standalone {
		records := s.Store.Records()
		if err := localProblem(hostname, records); err != nil {
			return err
		}
		return nomadProblem(hostname, records[hostname].Report.Nomad)
	}
	ignore := map[string]bool{}
	for h := range s.Store.Locks(now) {
		if h != hostname {
			ignore[h] = true
		}
	}
	_, err := s.checkSwarm(cfg, d.Swarm, ignore, s.Store.Records(), now)
	return err
}
