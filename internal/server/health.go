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

// swarmMembers returns every server which belongs to the Swarm, either by
// its last report or by the servers: section of the configuration.
func (s *Server) swarmMembers(cfg *fleet.Config, swarm string, records map[string]record, now time.Time) []member {
	seen := map[string]bool{}
	var out []member
	for h, rec := range records {
		d := s.desired(cfg, h, "", "", records, now)
		if d.Swarm == swarm {
			rec := rec
			out = append(out, member{h, &rec, d})
			seen[h] = true
		}
	}
	for h, srv := range cfg.Servers {
		if srv.Swarm == swarm && !seen[h] {
			out = append(out, member{h, nil, cfg.Resolve(h, "", "", now)})
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
	return v, nil
}

// canStartMaintenance decides if hostname may take a maintenance lock.
func (s *Server) canStartMaintenance(cfg *fleet.Config, d fleet.Desired, hostname string, locks map[string]Lock, records map[string]record, now time.Time) error {
	if d.Swarm == "" {
		return nil
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
// the server must be back in the Swarm and the whole Swarm healthy.
func (s *Server) canFinishMaintenance(cfg *fleet.Config, d fleet.Desired, hostname string, now time.Time) error {
	if d.Swarm == "" {
		return nil
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
