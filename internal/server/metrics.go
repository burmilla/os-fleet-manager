package server

import (
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
)

// Metrics are written by hand in the Prometheus text format to keep the
// service free of dependencies. All series are rebuilt on every scrape.

type metric struct {
	name, help, typ string
	samples         []string
}

func (m *metric) add(labels [][2]string, value float64) {
	var b strings.Builder
	b.WriteString(m.name)
	if len(labels) > 0 {
		b.WriteByte('{')
		for i, l := range labels {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "%s=%q", l[0], l[1])
		}
		b.WriteByte('}')
	}
	fmt.Fprintf(&b, " %g", value)
	m.samples = append(m.samples, b.String())
}

func (m *metric) write(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", m.name, m.help, m.name, m.typ)
	for _, s := range m.samples {
		fmt.Fprintln(w, s)
	}
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	var (
		syncTime = &metric{name: "burmilla_fleet_config_last_sync_timestamp_seconds", help: "Time of the last successful configuration sync.", typ: "gauge"}
		syncOK   = &metric{name: "burmilla_fleet_config_sync_success", help: "Whether the last configuration sync succeeded.", typ: "gauge"}
		syncFail = &metric{name: "burmilla_fleet_config_sync_failures_total", help: "Number of failed configuration syncs.", typ: "counter"}
		revision = &metric{name: "burmilla_fleet_config_info", help: "Revision of the active fleet configuration.", typ: "gauge"}
		info     = &metric{name: "burmilla_fleet_server_info", help: "Versions reported by a server.", typ: "gauge"}
		desired  = &metric{name: "burmilla_fleet_server_desired_info", help: "Target versions of a server.", typ: "gauge"}
		lastSeen = &metric{name: "burmilla_fleet_server_last_report_timestamp_seconds", help: "Time of the last report from a server, 0 if it never reported.", typ: "gauge"}
		upToDate = &metric{name: "burmilla_fleet_server_up_to_date", help: "Whether a server runs its target versions.", typ: "gauge"}
		window   = &metric{name: "burmilla_fleet_server_maintenance_window_active", help: "Whether the server is inside its maintenance window.", typ: "gauge"}
		state    = &metric{name: "burmilla_fleet_server_state", help: "Client state reported by a server.", typ: "gauge"}
		boot     = &metric{name: "burmilla_fleet_server_boot_timestamp_seconds", help: "Boot time reported by a server.", typ: "gauge"}
		restart  = &metric{name: "burmilla_fleet_server_restart_required", help: "Whether a scheduled restart of a server is due.", typ: "gauge"}
		pending  = &metric{name: "burmilla_fleet_server_maintenance_required", help: "Pending maintenance of a server by reason.", typ: "gauge"}
		lockHeld = &metric{name: "burmilla_fleet_server_maintenance_lock_held", help: "Whether a server holds a maintenance lock.", typ: "gauge"}
		locks    = &metric{name: "burmilla_fleet_maintenance_locks", help: "Maintenance locks held per Swarm and Environment.", typ: "gauge"}
		swHealth = &metric{name: "burmilla_fleet_swarm_healthy", help: "Whether the whole Swarm is healthy (servers in maintenance excluded).", typ: "gauge"}
		swNodes  = &metric{name: "burmilla_fleet_swarm_nodes", help: "Swarm nodes as seen by the freshest manager report.", typ: "gauge"}
		swMgrs   = &metric{name: "burmilla_fleet_swarm_managers", help: "Swarm managers as seen by the freshest manager report.", typ: "gauge"}
	)

	s.syncMu.Lock()
	if !s.lastSync.IsZero() {
		syncTime.add(nil, float64(s.lastSync.Unix()))
	}
	syncOK.add(nil, b2f(s.lastSyncOK))
	syncFail.add(nil, float64(s.syncFailures))
	s.syncMu.Unlock()
	if cfg := s.Config(); cfg != nil {
		revision.add([][2]string{{"revision", cfg.Revision}}, 1)
	}

	lockCount := map[[2]string]int{}
	for _, st := range s.statuses() {
		d := st.Desired
		base := [][2]string{{"hostname", st.Hostname}, {"swarm", d.Swarm}, {"environment", d.Environment}}
		with := func(extra ...[2]string) [][2]string {
			return append(append([][2]string{}, base...), extra...)
		}
		desired.add(with(
			[2]string{"os_version", d.OSVersion},
			[2]string{"docker_version", d.DockerVersion},
			[2]string{"upgrade_policy", d.UpgradePolicy},
		), 1)
		window.add(base, b2f(d.MaintenanceWindowActive))
		reason := ""
		if st.Lock != nil {
			reason = st.Lock.Reason
			lockCount[[2]string{d.Swarm, d.Environment}]++
		}
		lockHeld.add(with([2]string{"reason", reason}), b2f(st.Lock != nil))
		if st.Reported == nil {
			lastSeen.add(base, 0)
			continue
		}
		rep := st.Reported
		info.add(with(
			[2]string{"os_version", rep.OSVersion},
			[2]string{"docker_version", rep.DockerVersion},
			[2]string{"kernel_version", rep.KernelVersion},
			[2]string{"client_version", rep.ClientVersion},
		), 1)
		lastSeen.add(base, float64(st.LastSeen.Unix()))
		upToDate.add(base, b2f(st.UpToDate))
		restart.add(base, b2f(d.RestartRequired))
		for _, r := range []string{"os_upgrade", "docker_upgrade", "restart"} {
			pending.add(with([2]string{"reason", r}), b2f(slices.Contains(d.MaintenanceReasons, r)))
		}
		state.add(with([2]string{"state", rep.State}), 1)
		if !rep.BootTime.IsZero() {
			boot.add(base, float64(rep.BootTime.Unix()))
		}
	}
	keys := make([][2]string, 0, len(lockCount))
	for k := range lockCount {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i][0]+"/"+keys[i][1] < keys[j][0]+"/"+keys[j][1] })
	for _, k := range keys {
		locks.add([][2]string{{"swarm", k[0]}, {"environment", k[1]}}, float64(lockCount[k]))
	}

	for _, h := range s.swarmHealth() {
		l := [][2]string{{"swarm", h.Swarm}}
		swHealth.add(l, b2f(h.Healthy))
		if h.Status != nil {
			swNodes.add(append(l, [2]string{"state", "ready"}), float64(h.Status.NodesReady))
			swNodes.add(append(l, [2]string{"state", "down"}), float64(h.Status.Nodes-h.Status.NodesReady))
			swMgrs.add(append(l, [2]string{"state", "reachable"}), float64(h.Status.ManagersReachable))
			swMgrs.add(append(l, [2]string{"state", "unreachable"}), float64(h.Status.Managers-h.Status.ManagersReachable))
		}
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	for _, m := range []*metric{syncTime, syncOK, syncFail, revision, info, desired, lastSeen, upToDate, window, state, boot, restart, pending, lockHeld, locks, swHealth, swNodes, swMgrs} {
		m.write(w)
	}
}
