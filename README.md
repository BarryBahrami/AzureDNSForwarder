# Azure DNS Forwarder

A small, self-contained DNS forwarder with a built-in admin web UI. It was originally 
designed to run in an Azure VNet,
forwarding queries from VPN clients to Azure DNS (`168.63.129.16`) so that **Azure Private DNS
Zones** are resolvable from outside the VNet — for example, from VPN clients
or branch-office networks.  It has since been greatly expanded, to include forwarding to GCP and AWS VPCs, and to support **least-latency responses** from multiple upstreams.  Sync partners not only allow you to securely share config, but also to **automatically sync** the config of a pair of instances.  additionally, static records and indepentatly forwarding zones are supported.

Although the examples in this project put two DNS servers on a single host, I do not recommend this in production.  Instead, run two instances on separate hosts, and use the **Sync Partners** feature to keep them in sync.

This update is a work in progress.  Although I have tested much, there may be bugs.  Please report them.



* **Engine:** [unbound](https://www.nlnetlabs.nl/projects/unbound/about/)
* **Admin UI:** single static Go binary on port `80`
* **Config:** one YAML file, shared by every replica
* **Multi-instance:** high-availability pairs are first-class. Instances can
  share the file on a volume **or** exchange config securely over the
  built-in **Sync Partners** HTTPS API.

---

## Table of Contents

1. [Features](#features)
2. [Architecture](#architecture)
3. [Quick Start](#quick-start)
4. [Configuration File](#configuration-file)
5. [Using the Web UI](#using-the-web-ui)
6. [Sync Partners](#sync-partners)
7. [Least-Latency Responses](#least-latency-responses)
8. [API Reference](#api-reference)
9. [Security](#security)
10. [Operational Notes](#operational-notes)
11. [Troubleshooting](#troubleshooting)
12. [Future Work](#future-work)

---

## Features

| Feature | What it does |
|---|---|
| **Azure DNS catch-all** | Forwards every query to Azure DNS (`168.63.129.16`) by default. Editable, togglable, and removable. |
| **Wildcard forward zones** | A name prefixed with `*.` (e.g. `*.corp.local`) matches the zone and **every** subdomain, with no enumeration. |
| **Exact forward zones** | Forward a specific FQDN (e.g. `azure.contoso.com`) to one or more upstreams, tried in order. |
| **Least-latency responses** | For exact-match zones, periodically probe the upstream, measure latency to each returned record, and answer with the lowest-latency target(s). |
| **Static records** | Authoritative A / AAAA / CNAME records. Names can be FQDNs **or** single labels like `db`. |
| **Web UI** | Manage forwarders, default upstreams, records, settings, peers, import/export, audit, and a built-in resolve tool. |
| **YAML import / export** | Download the config for editing or version control; upload to replace it. |
| **Audit log** | Every admin and peer-driven change is recorded with actor and timestamp. |
| **In-browser resolve tool** | Query your own forwarder to see exactly what the network would receive. |
| **DNSSEC validation** | Off by default; one toggle in the UI. |
| **Hot-reload** | Config changes are picked up within the poll interval (default 10 s) on every instance. |
| **Sync Partners** | Optional peer-to-peer config exchange over HTTPS, authenticated by a preshared key, encrypted with a PSK-derived TLS certificate. |
| **Per-item and per-peer sync control** | Mark individual records / forwarders / defaults as local-only, or send them only to selected peers. |
| **Tombstones** | Deletes propagate across the peer mesh; last-writer-wins resolves conflicts. |

---

## Architecture

```text
┌─────────────────────────────────────────┐
│           dnsforwarderd (Go)            │
│  ┌─────────┐ ┌─────────┐ ┌──────────┐  │
│  │ Web UI  │ │Watcher  │ │ Sync     │  │
│  │ /api    │ │(reload) │ │ Partners │  │
│  └────┬────┘ └────┬────┘ └────┬─────┘  │
│       │           │           │         │
│       └───────────┴───────────┘         │
│              writes/reads               │
│            /config/dnsforwarder.yaml     │
│                    │                    │
│              generates                  │
│           /etc/unbound/dnsforwarder.conf │
│                    │                    │
│              supervises                 │
│              unbound (DNS)              │
└─────────────────────────────────────────┘
```

* The YAML file is the single source of truth.
* `dnsforwarderd` regenerates `unbound.conf` every poll cycle (or immediately
  after an admin change), validates it with `unbound-checkconf`, and reloads
  `unbound`.
* A loopback DNS proxy (`127.0.0.1:15353`) powers the least-latency feature
  for exact-match zones.
* The optional sync engine runs its own HTTPS listener (default `0.0.0.0:8443`)
  and pushes/pulls config from configured peers.

---

## Quick Start

### Build the image

```bash
docker build -t azure-dns-forwarder:local .
```

it can also be retrieved from dockerhub:  docker pull barrybahrami/azurednsforwarder

### Run a single instance (testing)

```bash
mkdir -p .shared-config
docker run --rm -d --name dnsfwd \
  -p 53:53/udp -p 53:53/tcp -p 8080:80 \
  --cap-add=NET_BIND_SERVICE \
  --cap-add=NET_RAW \
  -v $PWD/.shared-config:/config \
  azure-dns-forwarder:local
```

Or, pull the published image from Docker Hub:

```bash
mkdir -p .shared-config
docker run --rm -d --name dnsfwd \
  -p 53:53/udp -p 53:53/tcp -p 8080:80 \
  --cap-add=NET_BIND_SERVICE \
  --cap-add=NET_RAW \
  -v $PWD/.shared-config:/config \
  barrybahrami/azurednsforwarder
```

Open <http://localhost:8080> and start adding forwarders and records.

### One-liner with Sync Partners enabled

The same `docker run` form, but with the variables needed to bootstrap a peer.
Each peer still needs a persistent `/config` volume for its own copy of the
config and the audit log.

```bash
mkdir -p .my-config
docker run --rm -d --name dnsfwd \
  -p 53:53/udp -p 53:53/tcp \
  -p 8080:80 \
  -p 8443:8443 \
  --cap-add=NET_BIND_SERVICE \
  --cap-add=NET_RAW \
  -v $PWD/.my-config:/config \
  -e DNSFWD_INSTANCE=dnsfwd-site-a \
  -e PEER_LISTEN=0.0.0.0:8443 \
  -e PEER_SHARED_KEY="change-me-to-a-long-random-secret" \
  -e PEERS_INITIAL="https://dnsfwd-site-b.example.com:8443" \
  barrybahrami/azurednsforwarder
```

Repeat on a second host with the same `PEER_SHARED_KEY`, swapping the
`PEERS_INITIAL` URL for the first peer. After the first save on either peer,
config will sync in both directions automatically.

### Run an HA pair with shared storage

```bash
docker compose -f deploy/docker-compose.yml up -d
```

This launches two replicas (`dnsfwd-1` on `:53` / UI `:8081`, and `dnsfwd-2`
on `:8053` / UI `:8082`) sharing `./.shared-config/`. Configure clients to use
`dnsfwd-1` as primary and `dnsfwd-2` as secondary.

For Azure, replace the bind mount with an **Azure Files NFS** volume or a
**ReadWriteMany AKS PVC**.

### Run an HA pair with peer sync (no shared volume)

```bash
docker compose -f deploy/test-sync.yml up -d
```

This launches `test-a` and `test-b` with **separate** config directories. Each
instance publishes its own DNS and UI ports, and they exchange config over
HTTPS on `8443` using the shared key.

---

### Environment variables

All environment variables are optional; the config file takes precedence
after bootstrap.

| Variable | Purpose | Default |
|---|---|---|
| `DNSFWD_CONFIG` | Path to the YAML config file | `/config/dnsforwarder.yaml` |
| `DNSFWD_OUT` | Directory for generated `unbound.conf` | `/etc/unbound` |
| `DNSFWD_CONF_NAME` | Name of the generated unbound config | `dnsforwarder.conf` |
| `DNSFWD_UNBOUND_BIN` | Path to the `unbound` binary | `unbound` |
| `DNSFWD_INSTANCE` | Instance name in the audit log | hostname |
| `PEER_LISTEN` | Address for the peer sync HTTPS listener | `0.0.0.0:8443` |
| `PEER_SHARED_KEY` | Preshared key for peer sync | *(none)* |
| `PEER_SHARED_KEY_FILE` | File containing the PSK (Docker/k8s secret) | *(none)* |
| `PEERS_INITIAL` | Comma-separated peer URLs to bootstrap | *(none)* |

The `PEER_*` values seed the config **only when the fields are empty** in the
YAML, so you can rotate the key later by editing the file.

---

## Configuration File

The single source of truth is `/config/dnsforwarder.yaml` inside the
container. It is read on startup and polled every `settings.poll_seconds`.

### Schema

```yaml
version: 1
updated: 2026-06-14T22:15:10Z
updated_by: admin
settings:
  cache_size: 1000            # unbound msg-cache-size / rrset-cache-size
  dnssec: false               # enable DNSSEC validation
  log_queries: false          # increase unbound verbosity to log queries
  http_listen: 0.0.0.0:80     # admin UI / API bind address
  dns_listen: 0.0.0.0:53      # DNS bind address
  poll_seconds: 10            # how often to reload the YAML
peers:
  listen: 0.0.0.0:8443        # peer sync HTTPS bind address
  shared_key: "change-me"     # redacted on export
  sync_interval_seconds: 300  # how often to pull from each peer (floor: 10)
  clock_skew_seconds: 300     # accepted timestamp drift for peer envelopes
  list:
    - name: peer-1
      url: https://10.1.1.4:8443
      enabled: true
upstream_defaults:
  - address: 168.63.129.16
    port: 53
    enabled: true
    note: "Azure DNS"
    do_not_sync: false
    sync_peers: []              # empty = all peers
    updated_at: 2026-06-14T22:15:10Z
    updated_by: bootstrap
    deleted: false
forward_zones:
  - id: "<generated>"
    name: "*.corp.local"
    wildcard: true
    upstreams:
      - 10.0.0.4
      - 10.0.0.5
    least_latency: false
    latency_test_frequency: 5    # minutes, rounded up to nearest 5
    do_not_sync: false
    sync_peers: []
    updated_at: 2026-06-14T22:15:10Z
    updated_by: admin
    deleted: false
static_records:
  - id: "<generated>"
    name: db.corp.local
    type: A
    value: 10.1.2.3
    ttl: 300
    do_not_sync: false
    sync_peers: []
    updated_at: 2026-06-14T22:15:10Z
    updated_by: admin
    deleted: false
do_not_sync_settings: false      # keep the settings block local-only
```

### Validation rules

* `upstream_defaults` is required (may be empty). Each entry must be a valid
  IPv4/IPv6 address.
* At least one enabled upstream default **or** one forward zone must exist,
  otherwise every query would be refused.
* Forward zone names can be FQDNs **or** single labels. Wildcards must begin
  with `*.`.
* Forward zone upstreams may be IPs or hostnames.
* Static record names can be FQDNs or single labels. Values are validated by
  type (A = IPv4, AAAA = IPv6, CNAME = hostname).
* Duplicate non-deleted zones or records are rejected.
* Least latency is only allowed on **exact** (non-wildcard) forward zones.

---

## Using the Web UI

The UI is reachable at `/` and provides these pages:

| Page | URL | Purpose |
|---|---|---|
| **Dashboard** | `/` | Instance status, last reload, config hash, quick stats |
| **Forwarders** | `/forwarders` | Manage wildcard and exact forward zones |
| **Records** | `/records` | Manage static A / AAAA / CNAME records |
| **Settings** | `/settings` | Cache, DNSSEC, query logging, listen addresses, poll interval |
| **Sync Partners** | `/sync-partners` | Peers, PSK, sync interval, live peer status |
| **Import / Export** | `/import-export` | Download or upload the YAML config |
| **Audit** | `/audit` | Last 200 audit entries |

### Adding a wildcard forwarder

1. Go to **Forwarders** → **Add**.
2. Enter `*.corp.local` as the name; the UI sets `wildcard: true`.
3. Add upstreams, e.g. `10.0.0.4, 10.0.0.5`.
4. Save. Queries for `foo.corp.local`, `bar.baz.corp.local`, etc. now resolve
   through those upstreams.

### Adding a static record

1. Go to **Records** → **Add**.
2. Enter `db` (single label is fine), choose **A**, value `10.1.2.3`, TTL
   `300`.
3. Save. `db.<local-domain>` resolves to `10.1.2.3` from unbound.

### Enabling least latency

1. Create or edit an **exact** forward zone, e.g. `a.rtmp.youtube.com`.
2. Check **Least Latency Response**.
3. Set the test frequency in minutes (default 5).
4. Save. The internal proxy begins probing and returns the best-performing
   record(s).

### Resolving a name from the browser

Go to the **Dashboard** and use the **Resolve** tool, or call `/api/test`.
By default it queries the local unbound so you see exactly what clients on
this host would receive.

---

## Sync Partners

Sync Partners is the optional peer-to-peer configuration exchange. It is
useful when you want two instances with **separate** storage to stay in sync,
for example across availability zones or regions.

### How it works

* Each instance serves HTTPS on `peers.listen` (default `0.0.0.0:8443`).
* Every instance that shares the same `shared_key` can authenticate and verify
  the others.
* The local instance pulls from each peer every `sync_interval_seconds` and
  pushes on every local save.
* Items are filtered **at the source**: peers never see items marked
  `do_not_sync: true` or targeted at a different `sync_peers` list.

### Authentication and encryption

Peer traffic uses a TLS certificate **derived from the preshared key**. All
peers that share the key generate the same ECDSA public key, so clients verify
the listener by **public-key pinning** without a public CA, certificate files,
or persistent state. The PSK itself is sent in the `X-Peer-Token` header and
compared in constant time.

For defense in depth across untrusted networks, bind `PEER_LISTEN` to a
WireGuard interface only.

### Conflict resolution

* Last-writer-wins by per-item `updated_at`.
* If the local copy is newer, the incoming item is counted as a conflict and
  discarded.
* If timestamps tie and values match, the item is skipped.
* Deletes are tombstones (`deleted: true`) that propagate. A tombstone can
  be overwritten by a newer non-deleted item from another peer.

### Clock skew

`clock_skew_seconds` (default 300) rejects peer envelopes whose
`server_time` is outside the local clock window. Keep instance clocks in sync
with NTP.

### Bootstrap from environment

```yaml
services:
  dnsfwd:
    image: azure-dns-forwarder:local
    environment:
      - PEER_LISTEN=0.0.0.0:8443
      - PEER_SHARED_KEY=replace-me-with-a-long-random-secret
      - PEERS_INITIAL=https://10.2.1.4:8443,https://10.3.1.4:8443
    cap_add:
      - NET_BIND_SERVICE
      - NET_RAW
```

Or use Docker/k8s secrets:

```yaml
environment:
  - PEER_SHARED_KEY_FILE=/run/secrets/peer_key
```

### Recommended: WireGuard sidecar

The peer API is encrypted by the app, but it should still not travel over
the public internet. Add a WireGuard sidecar and bind the peer listener to the
WG interface:

```yaml
services:
  wg:
    image: lscr.io/linuxserver/wireguard:latest
    cap_add: [NET_ADMIN, SYS_MODULE]
    volumes:
      - ./wg:/config
  dnsfwd:
    image: azure-dns-forwarder:local
    network_mode: service:wg
    depends_on: [wg]
    environment:
      - PEER_LISTEN=10.99.0.1:8443
      - PEER_SHARED_KEY=${PEER_SHARED_KEY}
      - PEERS_INITIAL=https://10.99.0.2:8443
```

### Per-item sync control

Every mutable item (record, forwarder, upstream default) has a
**Do not sync with peers** checkbox. The settings block has a single global
**Do not sync settings** toggle. New items default to **sync**.

### Per-peer sync selection

Each item can carry a `sync_peers` list. When empty, the item syncs to every
peer. When populated, it is sent **only** to those named peers. Matching is
**case-insensitive** against `peers.list[].name`.

---

## Least-Latency Responses

For exact-match (non-wildcard) forward zones, the app can return the
lowest-latency answer instead of the full upstream response.

### How it works

1. Enable **Least Latency** on an exact forward zone.
2. The internal proxy periodically resolves the zone name through the
   configured upstream(s).
3. Every `latency_test_frequency` minutes it sends an ICMP echo request (ping)
   to each IP returned by the DNS lookup and records the round-trip time.
4. It caches the best-performing record(s) — all targets tied for lowest RTT
   are returned.
5. If **all** pings fail (for example because ICMP is blocked), the proxy falls
   back to returning the complete upstream answer, exactly like a regular
   forward zone.
6. `unbound` is configured to forward matching queries to the proxy at
   `127.0.0.1:15353`, so clients receive only the best targets.

### Configuration

* Only available on **exact** zones (`wildcard: false`).
* `latency_test_frequency` is stored and displayed in **minutes**. Any positive
  integer is accepted; the default is 5 minutes.
* Each peer runs its own independent measurements, so different instances may
  return different "best" records.
* The proxy is bound to loopback only and never accepts external traffic.

---

## API Reference

Include the header `X-Actor: <name>` on mutating requests to attribute changes
in the audit log.

### Admin API

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/status` | Instance, hash, last reload, last error |
| `GET` | `/api/healthz` | `200` if config is valid and applied |
| `GET` | `/api/config` | Full current config (PSK masked) |
| `PUT` | `/api/config` | Replace full config |
| `GET` | `/api/forwarders` | List active forward zones |
| `POST` | `/api/forwarders` | Add a forward zone |
| `PUT` | `/api/forwarders/{id}` | Update a forward zone |
| `DELETE` | `/api/forwarders/{id}` | Delete a forward zone |
| `GET` | `/api/defaults` | List catch-all upstreams |
| `POST` | `/api/defaults` | Add a catch-all upstream |
| `PATCH` | `/api/defaults/{addr}/{port}` | Update/enable/disable a default |
| `DELETE` | `/api/defaults/{addr}/{port}` | Delete a default |
| `GET` | `/api/records` | List static records |
| `POST` | `/api/records` | Add a record |
| `PUT` | `/api/records/{id}` | Update a record |
| `DELETE` | `/api/records/{id}` | Delete a record |
| `GET` | `/api/settings` | Get settings block |
| `PUT` | `/api/settings` | Update settings block |
| `GET` | `/api/peers` | Peer list + masked PSK length |
| `POST` | `/api/peers` | Add a peer |
| `PUT` | `/api/peers/{name}` | Update a peer |
| `DELETE` | `/api/peers/{name}` | Remove a peer |
| `POST` | `/api/peers/{name}/sync` | Reserved endpoint (sync is automatic) |
| `GET` | `/api/peers/status` | Live status of every peer |
| `GET` | `/api/export` | Download YAML; PSK redacted by default |
| `GET` | `/api/export?secrets=1` | Download YAML including the PSK |
| `POST` | `/api/import` | Upload YAML and replace config |
| `GET` | `/api/audit` | Last 200 audit entries |
| `POST` | `/api/test` | Resolve tool: `{name, qtype, upstream?}` |

#### Example: add a wildcard forwarder with curl

```bash
curl -s -X POST http://localhost:8080/api/forwarders \
  -H "Content-Type: application/json" \
  -H "X-Actor: cli" \
  -d '{
    "name": "*.corp.local",
    "wildcard": true,
    "upstreams": ["10.0.0.4", "10.0.0.5"],
    "least_latency": false,
    "do_not_sync": false
  }'
```

#### Example: add Azure DNS as a default upstream

```bash
curl -s -X POST http://localhost:8080/api/defaults \
  -H "Content-Type: application/json" \
  -H "X-Actor: cli" \
  -d '{"address": "168.63.129.16", "port": 53, "enabled": true, "note": "Azure DNS"}'
```

### Peer API

The peer API runs on `peers.listen` (default `8443`). All requests must use
`https://` and include `X-Peer-Token: <shared_key>`.

| Method | Path | Description |
|---|---|---|
| `GET` | `/peer/v1/manifest` | Counts and server time |
| `GET` | `/peer/v1/items?since=<seq>` | Full eligible envelope |
| `POST` | `/peer/v1/items` | Apply an envelope |
| `GET` | `/peer/v1/healthz` | Returns `200` if peer is up |

---

## Security

* **No built-in UI authentication.** The admin UI binds to all interfaces
  by default. Place it behind an NSG, a private VNet, or a reverse proxy with
  authentication.
* The container runs as the unprivileged `dnsfwd` user. It needs
  `NET_BIND_SERVICE` to bind port `53` and `NET_RAW` to send ICMP echo
  requests for least-latency probes. The Dockerfile also adds `SETUID`/
  `SETGID` so unbound can drop privileges correctly.
* **Peer PSK protection:** the shared key is redacted in API responses and in
  the default export. Use `?secrets=1` only when you need to back it up; treat
  that export as confidential.
* Peer traffic is encrypted with TLS using a PSK-derived certificate and
  public-key pinned. For untrusted networks, add a WireGuard sidecar.

---

## Operational Notes

* **Atomic writes:** the store writes to `*.tmp` and renames, so partial files
  are never observed by another poller.
* **Advisory locking:** GUI/API edits take an `flock`. If two writers collide,
  the second receives HTTP `409` and should retry.
* **Safe failed reloads:** if `unbound-checkconf` fails, the previous running
  config is left in place and the error appears in the UI footer and audit
  log.
* **Polling vs. immediate apply:** every instance polls the YAML every
  `poll_seconds`. Admin changes also trigger an immediate apply on the local
  instance.
* **Multi-instance edits:** add a rule on instance 1; instance 2 picks it up
  within `poll_seconds` when using shared storage, or near-instantly via peer
  sync.
* **unbound restart for local-data:** unbound's `SIGHUP` does not reload
  `local-data`, so the supervisor sends `SIGTERM` and restarts `unbound` when
  static records change. Queries may briefly be refused during restart.
* **Audit log:** written to `<config-dir>/audit.log`. The UI shows the last
  200 entries; older entries are not truncated automatically.

---

## Troubleshooting

| Symptom | Check |
|---|---|
| UI footer shows a red error | `/api/healthz` and `/api/status` return the last reload error; verify the YAML with `unbound-checkconf`. |
| Peer sync not happening | Verify `peers.shared_key` matches, `peers.list[].enabled` is true, URLs use `https://`, and clocks are within `clock_skew_seconds`. |
| Least-latency zone returns SERVFAIL | Check that the zone is exact (non-wildcard) and that the proxy has completed at least one probe (`/api/status`). |
| Least-latency zone returns all upstream records | ICMP is likely blocked or `NET_RAW` is missing. The proxy intentionally falls back to the full answer when pings fail. |
| Container cannot bind port 53 | Add `--cap-add=NET_BIND_SERVICE`. On rootless Docker you may also need a privileged port mapping. |
| Least-latency probes fail with "operation not permitted" | Add `--cap-add=NET_RAW` so the container can send ICMP echo requests. |
| DNS replies not reaching clients | Ensure unbound `access-control` allows the client network (the container already allows `0.0.0.0/0` and `::/0`). |

---

## Future Work

Features intentionally not implemented yet:

* HTTP basic auth or OIDC on the admin UI
* mTLS as an alternative to the PSK-derived certificate for peer auth
* Vector clocks for finer conflict resolution
* Import directly from Azure Private DNS Zones via managed identity
* Additional static record types: MX / TXT / SRV / PTR
* In-UI query log viewer

---

Built by Barry Bahrami — https://CloudInteriors.net

Released under GNU

Feel free to reach out with any questions.

LinkedIn: https://www.linkedin.com/in/barrybahrami/ Email: BarryBahrami at gmail

If this helped you then please consider donating to the San Diego Web Cam.

Bitcoin: bc1q0a5sf8q0j90qedndrmvgulv0rwxlfhc8rgk8c9

And watch on YouTube! SunDiegoLive.com

Thank you
