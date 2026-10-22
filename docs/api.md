# Fleet Manager API (v1)

The API is plain HTTP(S) + JSON. Authentication is optional: a Swarm +
Environment with `htpasswd` entries in `fleet.yml` requires HTTP Basic
authentication with one of them on `GET /api/v1/config`, `POST /api/v1/report`
and the maintenance lock endpoints.

* Credentials decide the Swarm + Environment of the request; the `swarm` /
  `environment` sent by the client are ignored.
* A hostname that belongs to a protected Swarm + Environment, by the
  `servers:` section or by its earlier reports, needs that one's credentials:
  `401` without credentials, `403` with credentials of another one.
* Wrong credentials give `401` with `WWW-Authenticate: Basic`.
* `/api/v1/servers`, `/api/v1/swarms`, `/metrics` and `/healthz` are open.

All timestamps are RFC 3339 in UTC.

## `GET /api/v1/config`

Returns the desired configuration for one server.

Query parameters:

| Name          | Required | Description                                                     |
|---------------|----------|-----------------------------------------------------------------|
| `hostname`    | yes      | Server hostname. This is the server identity in the fleet.      |
| `swarm`       | no       | Swarm the client thinks it belongs to (`FLEET_SWARM`).          |
| `environment` | no       | Environment the client thinks it belongs to (`FLEET_ENVIRONMENT`). |

If the server is listed under `servers:` in `fleet.yml` and that entry sets
`swarm` / `environment`, those values win over what the client sent.

Response `200`:

```json
{
  "hostname": "node-01",
  "swarm": "swarm-a",
  "environment": "production",
  "known": true,
  "os_image": "burmilla/os",
  "os_version": "v3.0.0",
  "docker_version": "docker-29.9.2",
  "upgrade_policy": "auto",
  "check_interval_seconds": 300,
  "maintenance_window": {
    "days": ["sat", "sun"],
    "start": "02:00",
    "duration": "3h",
    "timezone": "Europe/Helsinki"
  },
  "maintenance_window_active": false,
  "maintenance_window_ends": "",
  "maintenance_window_next_start": "2026-10-24T23:00:00Z",
  "max_concurrent_maintenance": 1,
  "max_uptime_seconds": 7776000,
  "restart_after": "",
  "ssh": {
    "authorized_keys": ["ssh-ed25519 AAAA... alice@example.com"],
    "password_authentication": false,
    "disable_after_boot": "10m0s"
  },
  "config_revision": "3f2c1a9...",
  "ssh_in_sync": true,
  "restart_required": false,
  "maintenance_required": true,
  "maintenance_reasons": ["os_upgrade"]
}
```

* `known` is `true` when the hostname is listed under `servers:`.
* `os_version` is the tag of `os_image` (`ros os upgrade -i <os_image>:<os_version>`).
  Empty means "do not manage the OS version".
* `docker_version` is a User Docker engine name as listed by `ros engine list`
  (`ros engine switch <docker_version>`). Empty means "do not manage the engine".
* `upgrade_policy` is one of:
  * `auto` – apply upgrades and scheduled restarts inside the maintenance
    window, after taking a maintenance lock,
  * `download` – only pre-pull the target images,
  * `none` – only report status.
* `maintenance_window` is `null` when no window is configured. In that case
  `maintenance_window_active` is always `true` (changes are allowed at any time).
* `maintenance_window_ends` is set while the window is active.
* `restart_required`, `maintenance_required` and `maintenance_reasons`
  (`os_upgrade`, `docker_upgrade`, `restart`) are evaluated against the last
  report of the server. A restart is due when the server has been up longer
  than `max_uptime_seconds`, or when it booted before `restart_after` (a
  one-off restart request) and that time has passed.
* `ssh.authorized_keys` is the complete list of keys the client manages for
  the `rancher` and `docker` users; `null` means "do not manage keys" and `[]`
  removes all managed keys. `ssh.password_authentication` is `null` when not
  managed. SSH settings are applied right away, without a maintenance lock.
* `ssh.disable_after_boot` (Go duration, omitted when not managed): stop
  sshd once the server has been up this long and no SSH session is active.
  `0s` enables sshd.
* `ssh_in_sync` tells whether the SSH state of the last report matches `ssh`.

`400` if `hostname` is missing. `503` if no configuration has been loaded yet.

## `POST /api/v1/report`

Clients report their current state. Body:

```json
{
  "hostname": "node-01",
  "swarm": "swarm-a",
  "environment": "production",
  "os_version": "v3.0.0",
  "docker_version": "docker-29.9.2",
  "kernel_version": "6.12.50-burmilla",
  "client_version": "v3.0.0",
  "boot_time": "2026-10-20T08:12:44Z",
  "state": "idle",
  "message": "",
  "swarm_status": {
    "node_id": "x1y2z3",
    "cluster_id": "c0ffee",
    "local_node_state": "active",
    "manager": true,
    "nodes": 5,
    "nodes_ready": 5,
    "managers": 3,
    "managers_reachable": 3,
    "services": 12,
    "services_ready": 11,
    "services_not_ready": [
      {"name": "web", "mode": "replicated", "desired_tasks": 3, "running_tasks": 2, "update_state": ""}
    ]
  },
  "containers": {
    "containers": 3,
    "containers_ready": 2,
    "containers_not_ready": [
      {"name": "db", "state": "running", "health": "starting", "restart_policy": "always", "exit_code": 0}
    ]
  },
  "ssh": {
    "authorized_keys": ["ssh-ed25519 AAAA... alice@example.com"],
    "password_authentication": false,
    "disabled": true
  }
}
```

`swarm_status` comes from the User Docker `/info` endpoint, and on managers
also from `/nodes` and `/services?status=true`. It is omitted when User Docker
is not reachable. Node and service counters are only filled in by managers.
A service is ready when it runs all desired tasks (Swarm reports a task with
a health check as running only once it is healthy) and no update is in
progress (`updating`, `paused`, `rollback_started`, `rollback_paused`), i.e.
all tasks run the target configuration. Jobs are skipped.

`containers` covers the local User Docker containers of the server which
Docker restarts on its own (restart policy `always`, `unless-stopped` or
`on-failure`); Swarm tasks and containers labelled
`io.burmilla.fleet.ignore=true` are left out. A container is ready when it
runs and, if it has a health check, is `healthy`; an `on-failure` container
which exited with code 0 is done. Every server reports them, inside a Swarm
or not. Omitted when User Docker is not reachable; such reports do not block
maintenance.

`ssh` is the applied SSH state: the keys in the block managed by the client
(`null` when it does not manage keys) and whether sshd accepts passwords
(`sshd -T`), and `disabled` when sshd is disabled
(`ros console sshd status`). It is omitted when the client could not read it.

`state` is one of `idle`, `downloading`, `upgrading`, `waiting`, `error`
(free text is accepted, these are the values the bundled client sends).

If the server holds a maintenance lock, no maintenance is required any more,
its local containers are ready and the whole Swarm is healthy again (see
below), the lock is released.

Response `200` with the same document as `GET /api/v1/config` so a client can
report and fetch in one round trip.

## `POST /api/v1/maintenance/lock`

Takes a maintenance slot before an upgrade or a restart. Body:

```json
{"hostname": "node-01", "swarm": "swarm-a", "environment": "production", "reason": "os_upgrade"}
```

The lock is granted only when all of these hold:

1. The server is inside its maintenance window.
2. Fewer than `max_concurrent_maintenance` servers of the same Swarm +
   Environment hold a lock.
3. The whole Swarm is healthy. Servers that hold a lock are allowed to be down,
   every other server of the Swarm (by report or by the `servers:` section)
   must:
   * have reported within 3 × its check interval + 1 minute,
   * not report `state: error`,
   * report `local_node_state: active` and the same Swarm cluster ID.

   The freshest manager report must not show more nodes down / managers
   unreachable than there are lock holders, and every service must be
   ready: running all desired tasks with its target configuration. A global
   service may miss one task per server in maintenance; replicated services
   must have been rescheduled to the remaining nodes. Without any manager
   report the Swarm counts as unhealthy.
4. If the requester is a manager, taking it down must keep the Raft quorum
   (with 3+ managers), or no other manager may be unavailable (1–2 managers).

Servers that are not part of a Swarm (empty `swarm`) only need 1 and 2.

* `200` – granted (or renewed if the host already held it):
  `{"granted": true, "expires": "2026-10-25T00:30:00Z", "holders": ["node-01"]}`
* `409` – refused, with the reason:
  `{"granted": false, "holders": ["node-02"], "message": "1 nodes of Swarm swarm-a are not ready (seen by mgr-01)"}`

A lock is released when the holder reports that nothing is required any more
**and** the whole Swarm is healthy again, so the next server only starts after
the previous one has fully rejoined. Locks also expire after `FLEET_LOCK_TTL`
(default `30m`); a server that is still broken after that keeps blocking the
Swarm through the health check.

## `DELETE /api/v1/maintenance/lock?hostname=node-01`

Releases the lock held by the host (manual override). `204`.

Locks are stored in `$FLEET_DATA_DIR/locks.json` and survive a restart of
Fleet Manager. A lock is granted only after it has been written to disk.

## `GET /api/v1/servers`

Everything the manager knows, for humans and scripts:

```json
[
  {
    "hostname": "node-01",
    "known": true,
    "reported": { "...": "last POST /api/v1/report body" },
    "last_seen": "2026-10-22T12:00:00Z",
    "remote_addr": "10.0.0.11",
    "desired": { "...": "GET /api/v1/config response" },
    "up_to_date": true,
    "lock": null
  }
]
```

`lock` is `{"reason": "os_upgrade", "expires": "..."}` while the server holds a
maintenance lock.

## `GET /api/v1/swarms`

Health of every Swarm as used by the lock decisions:

```json
[
  {
    "swarm": "swarm-a",
    "healthy": false,
    "message": "worker-03 has not reported since 2026-10-22T10:00:00Z",
    "servers": ["mgr-01", "mgr-02", "mgr-03", "worker-03"],
    "status": { "...": "swarm_status of the freshest manager report" }
  }
]
```

## `GET /metrics`

Prometheus text format. See the README for the metric list.

## `GET /healthz`

`200 ok` once a configuration has been loaded, `503` before that.
