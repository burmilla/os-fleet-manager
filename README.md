# BurFleet

BurFleet is a singleton service to control large amounts of BurmillaOS installations.

It is the successor of the RancherOS `os-subscriber` idea, built for servers
which are part of a Docker Swarm:

* The desired state of the fleet lives in a **private Git repository**
  (GitHub or Azure DevOps), so every change is reviewed and versioned.
* Target **BurmillaOS** and **User Docker** versions are set per **Swarm +
  Environment** (and can be overridden per server).
* Every server has a **maintenance window**. Upgrades and **scheduled
  restarts** only happen inside it.
* Servers take a **maintenance lock** before they upgrade or restart. Locks are
  only handed out while the **whole Swarm is healthy**, including every
  service running all its tasks with its target configuration, and are only released
  once the server is back and the Swarm is healthy again, so a rollout never
  takes down more nodes than you allow and stops on the first broken node.
* Optional htpasswd-style **authentication per Swarm + Environment**.
* Servers report back their state; BurFleet publishes it as
  **Prometheus metrics**, with example **Grafana dashboards**.

The client side is the optional `fleet-client` system service in
[os-services](https://github.com/burmilla/os-services) (`images/20-fleetclient`).
[Nomad QEMU nodes](https://github.com/olljanat/nomad-qemu-node) have their own
[fleet-client](https://github.com/olljanat/nomad-qemu-node/tree/master/fleet-client),
see [Nomad QEMU nodes](#nomad-qemu-nodes).

```
 private Git repo ──(git fetch every minute)──► BurFleet ◄── Prometheus ◄── Grafana
   fleet.yml                                     ▲      │
                                     report/lock │      │ target versions, window, restart
                                                 │      ▼
                                        fleet-client on every BurmillaOS server
```

## Fleet configuration

`fleet.yml` in the root of the configuration repository. Settings are
inherited `defaults` < `swarms.<swarm>` < `swarms.<swarm>.environments.<env>`
< `swarms.<swarm>.servers.<hostname>` < `swarms.<swarm>.environments.<env>.servers.<hostname>`. See [examples/fleet-config/fleet.yml](examples/fleet-config/fleet.yml).

| Setting                      | Description |
|------------------------------|-------------|
| `os_image`                   | BurmillaOS image repository, default `burmilla/os` |
| `os_version`                 | Target BurmillaOS version (image tag), e.g. `v3.0.0`. Empty = not managed |
| `docker_version`             | Target User Docker engine as listed by `ros engine list`, e.g. `docker-29.9.2`. Empty = not managed |
| `upgrade_policy`             | `auto` (apply in maintenance window), `download` (pre-pull only) or `none` (report only, the default) |
| `maintenance_window`         | `days` (`mon`..`sun`, empty = every day), `start` (`HH:MM`), `duration` (Go duration) and `timezone` (IANA, default UTC). No window = any time |
| `max_concurrent_maintenance` | How many servers of one Swarm + Environment may be in maintenance at once, default 1 |
| `max_uptime`                 | Scheduled restart when uptime exceeds this, e.g. `30d` or `720h` |
| `restart_after`              | One-off restart: servers booted before this RFC 3339 time restart in their next window |
| `check_interval`             | How often clients check in, default `5m` |
| `ssh.authorized_keys`        | SSH public keys of the `rancher` and `docker` users (`root` on Nomad QEMU nodes) managed by fleet-client. The list of the most specific level wins. `[]` removes all managed keys, unset = not managed |
| `ssh.password_authentication`| `true` / `false` enables or disables password logins in sshd. Unset = not managed |
| `ssh.disable_after_boot`     | Hardening: stop sshd once the server has been up this long (e.g. `10m`) and no SSH session is active. `0` = enable sshd, unset = not managed |

A server's Swarm and Environment come only from the client: fleet-client
calls `/api/v1/swarms/<FLEET_SWARM>/<FLEET_ENVIRONMENT>/...`. The
configuration never moves a server, so the settings of a Swarm or
environment, including the `servers:` entries under it, only apply to servers
which call that path. A `servers:` entry only adjusts settings of a member,
e.g. its own maintenance window:

```yaml
swarms:
  swarm-a:
    environments:
      production:
        servers:
          swarm-a-mgr-01:
            maintenance_window: {days: [sat], start: "02:00", duration: 1h}
servers:              # servers which are not in any Swarm (/api/v1/...)
  legacy-01:
    upgrade_policy: none
```

Servers which are not members of a Docker Swarm can be grouped with a
`standalone: true` entry under `swarms:` (clients use `FLEET_SWARM=<group>`).
Such a group has environments, `htpasswd` and maintenance slots like a Swarm,
but before and after maintenance only the server itself is checked (its local
containers), not the other members. Servers without any Swarm or group call
`/api/v1/...` and only get the defaults and the top-level `servers:` entries.

### Splitting the configuration per team

Swarms can be kept in their own files next to `fleet.yml`, so the parts can
be owned by different teams with CODEOWNERS (GitHub) or path-based required
reviewers (Azure DevOps) and branch protection:

```
fleet.yml                          defaults and servers outside Swarms (platform team)
swarms/swarm-a.yml                 Swarm settings and environments (team A)
swarms/swarm-a/production.yml      one environment (e.g. production ops)
```

* `swarms/<swarm>.yml` has the same fields as a `swarms.<swarm>` entry.
* `swarms/<swarm>/<environment>.yml` has the fields of an environment
  (including `htpasswd` and `servers:`).
* A Swarm or environment may be defined only once across all files, so one
  team cannot override what another team's file defines. Because membership
  comes from the clients, a team's file only affects servers which call that
  team's Swarm + Environment; it cannot pull in or change servers of others.
* If any file is invalid, BurFleet keeps serving the previous
  configuration (`burmilla_fleet_config_sync_success` is 0). Run
  `fleet-manager validate fleet.yml` in CI of the configuration repository
  so broken changes are caught before merge.

See [examples/fleet-config](examples/fleet-config) for a split configuration
with [CODEOWNERS](examples/fleet-config/CODEOWNERS) and a
[validation workflow](examples/fleet-config/.github/workflows/validate.yml).

### Existing installations: visibility first

fleet-client also runs on older BurmillaOS and RancherOS 1.x installations
(see its [README](https://github.com/burmilla/os-services/blob/v3.0.x/images/20-fleetclient/README.md)).
To see the state of a fleet before planning upgrades, start with a
report-only configuration like
[examples/visibility/fleet.yml](examples/visibility/fleet.yml):
`upgrade_policy: none` and no `ssh`, `max_uptime` or `restart_after`, so
clients only report. The target versions show who is behind in the metrics
and dashboards.

### SSH access

fleet-client applies `ssh` settings on every check-in, without a maintenance
lock, because they do not interrupt running workloads:

* `authorized_keys` are written to a block marked
  `# BEGIN burmilla-fleet-manager` / `# END burmilla-fleet-manager` in
  `~/.ssh/authorized_keys` of the `rancher` and `docker` users. Keys outside
  of that block (cloud-config `ssh_authorized_keys`, added by hand) are left
  alone, so removing a key from `fleet.yml` removes it only from the block.
* `password_authentication` is written to
  `/etc/ssh/sshd_config.d/50-burmilla-fleet.conf` in the console container
  and sshd is reloaded. Open sessions are kept.

* `disable_after_boot` stops sshd of the console (`ros console sshd
  disable --if-idle`) once the server has been up that long and no SSH
  connection is established. While a session is open fleet-client checks
  again every minute. sshd starts again after every boot, so SSH is
  available for the same period after a reboot; `0` enables sshd right away
  on running servers (console access works as well). Needs a BurmillaOS
  version with `ros console sshd`.

Make sure servers have working keys before disabling passwords. The applied
state is reported back and exposed as `burmilla_fleet_server_ssh_in_sync`.

```yaml
defaults:
  ssh:
    authorized_keys:
      - ssh-ed25519 AAAA... alice@example.com
swarms:
  swarm-a:
    environments:
      production:
        ssh:
          password_authentication: false
```

### Authentication

The API can stay open, but each Swarm + Environment can be protected with
htpasswd-style HTTP Basic authentication:

```yaml
swarms:
  swarm-a:
    environments:
      production:
        htpasswd:
          - "swarm-a-production:$2y$05$..."   # htpasswd -nbB swarm-a-production 'password'
```

* Only bcrypt entries are accepted, and a user name can be used by one Swarm +
  Environment only. Several entries allow rotating passwords.
* Credentials are valid only for the paths of their own Swarm + Environment.
* A server which reports to a protected Swarm + Environment cannot be claimed
  through another path (to send reports or take locks for it) until it has
  stopped reporting there for three check intervals. A server that really
  moves just waits that long.
* Unprotected Swarm + Environments, and `/api/v1/servers`, `/api/v1/swarms`
  and `/metrics`, stay open.

Basic authentication sends the password with every request, so use TLS:
set `FLEET_TLS_CERT` / `FLEET_TLS_KEY` or put BurFleet behind a TLS
reverse proxy.

What it protects and what not:

* The bcrypt hashes can be kept in the configuration repository, like
  `/etc/shadow` entries: they cannot be reversed, only guessed. Use long
  random passwords (e.g. `openssl rand -base64 24`), which make guessing
  infeasible even for someone who can read the repository.
* The password is shared by all servers of the Swarm + Environment and stored
  on each of them (`rancher.environment.FLEET_PASSWORD`). Whoever has root on
  one of them can act as any server of that environment: send false reports
  or take its maintenance locks. It cannot reach other environments.
* It authenticates servers towards BurFleet, not BurFleet towards
  servers: use HTTPS (and `FLEET_CA_CERT` for a private CA) so clients only
  take configuration from the real BurFleet.
* Configuration itself is not secret (`/api/v1/servers` stays open); do not
  put secrets into `fleet.yml`.

### Nomad QEMU nodes

Since v1.2.0, [Nomad QEMU nodes](https://github.com/olljanat/nomad-qemu-node) (Ubuntu based
elemental A/B images running QEMU VMs under HashiCorp Nomad) report with
`node_type: nomad-qemu-node`, their OS image tag as `os_version` and their
QEMU and Nomad versions. Their
[fleet-client](https://github.com/olljanat/nomad-qemu-node/tree/master/fleet-client)
drains the node in Nomad, runs `elemental upgrade` to
`<os_image>:<os_version>` and reboots, while it holds a maintenance lock.

Put them into their own group:

```yaml
swarms:
  nomad-qemu:
    standalone: true                             # no Docker Swarm
    os_image: ghcr.io/olljanat/nomad-qemu-node   # the default for these nodes
    os_version: v20261024-1                      # release tag
    upgrade_policy: auto
    environments:
      production: {}
```

* `os_image` and `os_version` of `defaults` describe BurmillaOS and are not
  applied to them; `os_image` defaults to `ghcr.io/olljanat/nomad-qemu-node`.
  `docker_version` does not apply. `ssh` settings apply to `root`
  (fleet-client of nomad-qemu-node with SSH support): keys go to
  `/root/.ssh/authorized_keys`, `disable_after_boot` stops `ssh.socket`.
* Mark their group `standalone: true` (a reporting Nomad QEMU node is
  handled as standalone anyway, but servers listed in the group which have
  not reported yet would otherwise count as Swarm members). Instead of the
  Swarm health check, a maintenance lock is only granted while every other
  Nomad QEMU node of the group (which does not hold a lock) reports, is not
  in `error` and has a Nomad node which is `ready`, eligible for scheduling
  and not draining, so the VMs have somewhere to go.
* The lock is released once the node runs the target version and its Nomad
  node is ready and eligible again.
* Since v1.3.0 they report the status of their software RAID arrays
  (`/proc/mdstat`): degraded arrays, failed and spare devices and the
  progress of a rebuild show on the dashboard.
* Grafana dashboard: [grafana/nomad-qemu-nodes.json](grafana/nomad-qemu-nodes.json).
  The BurmillaOS fleet overview leaves these nodes out.

![Nomad QEMU nodes dashboard](grafana/nomad-qemu-nodes.png)

### Maintenance lock and Swarm health

Before an upgrade or restart a client asks for a maintenance lock. It is granted
only when the server is inside its window, a slot is free in its Swarm +
Environment, and the whole Swarm is healthy: every other member (that does not
hold a lock) has reported recently, is `active` in the Swarm and not in error,
and the freshest manager report shows no more nodes down or managers
unreachable than there are servers in maintenance, and **every service runs all
of its desired tasks, healthy and with its target configuration** (no update in
progress, paused or rolling back; a global service may miss the tasks of the
servers in maintenance), and **every local container with a restart policy**
(`always`, `unless-stopped`, `on-failure`; not Swarm tasks) on those servers
runs and, with a health check, is healthy. Managers additionally must not break
the Raft quorum. Servers outside of a Swarm only need their own local
containers to be ready. The lock is released when the server reports the target
state **and** its local containers and the Swarm are healthy again. Label a
container `io.burmilla.fleet.ignore=true` to leave it out of the check. Details are in
[docs/api.md](docs/api.md); `GET /api/v1/swarms` shows why a Swarm is blocked.

Because of this, run `fleet-client` on all members of a Swarm, managers
included.

## Running

```bash
docker run -d --name fleet-manager -p 8080:8080 -v fleet-data:/data \
  -e FLEET_GIT_URL=https://github.com/example/fleet-config.git \
  -e FLEET_GIT_TOKEN=github_pat_... \
  burmilla/os-fleet-manager:v1.3.0
```

For Docker Swarm see [deploy/docker-stack.yml](deploy/docker-stack.yml). It runs
BurFleet hardened: as an unprivileged user without Linux capabilities, on a
read-only root filesystem with only `/data` and a small `/tmp` writable, with
HTTPS, secrets readable by the service user only, and memory/CPU limits.
Run a **single replica** with a persistent `/data` volume: the last reports
and the maintenance locks are stored there (`reports.json`, `locks.json`), so
a restart or a reschedule of the service keeps the locks. A lock is only
granted once it has been written to disk.

| Variable               | Default      | Description |
|------------------------|--------------|-------------|
| `FLEET_GIT_URL`        |              | HTTPS URL of the configuration repository |
| `FLEET_GIT_BRANCH`     | `main`       | Branch to follow |
| `FLEET_GIT_TOKEN`      |              | Personal access token (read-only access to the repository is enough) |
| `FLEET_GIT_TOKEN_FILE` |              | Read the token from a file, e.g. a Docker secret |
| `FLEET_GIT_USERNAME`   | `git`        | User name sent with the token |
| `FLEET_GIT_PATH`       |              | Directory of the configuration inside the repository, e.g. `ops/fleet`, when the repository is used for other things too. Only that directory is checked out |
| `FLEET_CONFIG_FILE`    | `fleet.yml`  | Configuration file inside `FLEET_GIT_PATH` (or `FLEET_CONFIG_DIR`); the `swarms` directory is read next to it |
| `FLEET_CONFIG_DIR`     |              | Use a local directory instead of Git |
| `FLEET_SYNC_INTERVAL`  | `1m`         | How often the repository is fetched |
| `FLEET_LOCK_TTL`       | `30m`        | Maintenance lock lifetime |
| `FLEET_DATA_DIR`       | `/data`      | Repository checkout, last reports and maintenance locks |
| `FLEET_LISTEN`         | `:8080`      | Listen address |
| `FLEET_TLS_CERT`, `FLEET_TLS_KEY` |   | Serve HTTPS with this certificate and key |

The configuration can live in a directory of a repository that is used for
other things too, e.g. an infrastructure monorepo: set `FLEET_GIT_PATH` to that
directory. Only that directory is checked out (sparse checkout, and a
blobless partial clone where the server supports it, as GitHub and Azure
DevOps do). Keep in mind:

* Protect the directory with CODEOWNERS or path-based required reviewers,
  since anyone who can merge to the branch can change the fleet.
* The token can still read the whole repository; GitHub and Azure DevOps
  tokens cannot be limited to one directory.
* The configuration revision is the commit of the branch, so it also changes
  with commits outside the directory.

Tokens:

* **GitHub**: a fine-grained personal access token with *Contents: read-only*
  on the configuration repository.
* **Azure DevOps**: a PAT with *Code (Read)* scope. URL format
  `https://dev.azure.com/<org>/<project>/_git/<repo>`; old
  `https://<org>.visualstudio.com/...` URLs work too.

The token is passed to git through the environment by a credential helper and
is never written to disk. A token file may end with a newline and may come
from Windows (byte order mark, CRLF). A rejected token shows up as
`Authentication failed` in the log; check the scope and expiry of the token. If a sync or the new configuration is invalid, the last
good configuration stays active and `burmilla_fleet_config_sync_success` drops
to 0.

## API

The API only serves versions and maintenance windows, so authentication is
optional, per Swarm + Environment (see above). Full contract:
[docs/api.md](docs/api.md).

`{base}` is `/api/v1/swarms/<swarm>/<environment>` for members of a Swarm or
standalone group, and `/api/v1` for servers outside of them.

| Endpoint                            | Use |
|-------------------------------------|-----|
| `GET {base}/config?hostname=…`      | Desired state of a server |
| `POST {base}/report`                | Client status report, returns the desired state |
| `POST {base}/maintenance/lock`      | Take a maintenance lock |
| `DELETE {base}/maintenance/lock`    | Release a lock manually |
| `GET /api/v1/servers`               | All servers with reported and desired state |
| `GET /api/v1/swarms`                | Swarm health as used for lock decisions |
| `GET /metrics`                      | Prometheus metrics |
| `GET /healthz`                      | Health check |

## Metrics and dashboards

| Metric | Labels |
|--------|--------|
| `burmilla_fleet_server_info` | hostname, swarm, environment, os_version, docker_version, kernel_version, client_version (Nomad QEMU nodes also qemu_version, nomad_version) |
| `burmilla_fleet_server_desired_info` | hostname, swarm, environment, os_version, docker_version, upgrade_policy |
| `burmilla_fleet_server_up_to_date` | hostname, swarm, environment |
| `burmilla_fleet_server_maintenance_required` | hostname, swarm, environment, reason |
| `burmilla_fleet_server_restart_required` | hostname, swarm, environment |
| `burmilla_fleet_server_maintenance_window_active` | hostname, swarm, environment |
| `burmilla_fleet_server_maintenance_lock_held` | hostname, swarm, environment, reason |
| `burmilla_fleet_server_state` | hostname, swarm, environment, state |
| `burmilla_fleet_server_last_report_timestamp_seconds` | hostname, swarm, environment (0 = never reported) |
| `burmilla_fleet_server_boot_timestamp_seconds` | hostname, swarm, environment |
| `burmilla_fleet_server_containers` | hostname, swarm, environment, state (local containers with a restart policy: ready / not_ready) |
| `burmilla_fleet_server_container_not_ready` | hostname, swarm, environment, container, state, health |
| `burmilla_fleet_server_ssh_in_sync` | hostname, swarm, environment (managed SSH keys and password setting applied) |
| `burmilla_fleet_server_ssh_password_authentication` | hostname, swarm, environment (sshd accepts passwords) |
| `burmilla_fleet_server_ssh_disabled` | hostname, swarm, environment (sshd disabled by `disable_after_boot`) |
| `burmilla_fleet_server_nomad_node` | hostname, swarm, environment, status, eligibility, drain (Nomad QEMU nodes) |
| `burmilla_fleet_server_nomad_allocations` | hostname, swarm, environment (running Nomad allocations, Nomad QEMU nodes) |
| `burmilla_fleet_server_raid_array` | hostname, swarm, environment, array, level, state (software RAID arrays) |
| `burmilla_fleet_server_raid_degraded` | hostname, swarm, environment, array (active array misses devices) |
| `burmilla_fleet_server_raid_devices` | hostname, swarm, environment, array, state (active / missing / failed / spare) |
| `burmilla_fleet_server_raid_sync_progress_percent` | hostname, swarm, environment, array, action (while a recovery, resync, check, repair or reshape runs) |
| `burmilla_fleet_maintenance_locks` | swarm, environment |
| `burmilla_fleet_swarm_healthy` | swarm |
| `burmilla_fleet_swarm_unhealthy_reason` | swarm, reason (first problem found, only for unhealthy Swarms) |
| `burmilla_fleet_swarm_nodes` / `burmilla_fleet_swarm_managers` / `burmilla_fleet_swarm_services` | swarm, state |
| `burmilla_fleet_swarm_service_running_tasks` / `burmilla_fleet_swarm_service_desired_tasks` | swarm, service, mode, update_state (services which are not ready) |
| `burmilla_fleet_config_info` | revision |
| `burmilla_fleet_config_last_sync_timestamp_seconds`, `burmilla_fleet_config_sync_success`, `burmilla_fleet_config_sync_failures_total` | |

Per-server metrics of servers which report a node type (Nomad QEMU nodes)
also have a `node_type` label.

Scrape config example: [deploy/prometheus.yml](deploy/prometheus.yml).
Grafana dashboards to import: [grafana/fleet-overview.json](grafana/fleet-overview.json)
(BurmillaOS fleet), [grafana/fleet-server.json](grafana/fleet-server.json) (one server)
and [grafana/nomad-qemu-nodes.json](grafana/nomad-qemu-nodes.json) (Nomad QEMU nodes).

![Fleet overview dashboard](grafana/fleet-overview.png)

## Development

```bash
go test ./...
docker build -t burmilla/os-fleet-manager:dev .
FLEET_CONFIG_DIR=examples/fleet-config FLEET_DATA_DIR=/tmp/fleet go run .
```

## License

Copyright (c) 2026 Project Burmilla

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

[http://www.apache.org/licenses/LICENSE-2.0](http://www.apache.org/licenses/LICENSE-2.0)

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
