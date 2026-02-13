# Proxy Pool

English | [简体中文](README_ZH.md)

Proxy Pool is a self-hosted proxy node pool manager built on top of [sing-box](https://github.com/SagerNet/sing-box).
It converts your nodes/subscriptions into one or more **HTTP proxy entry points**, with health checks, failover, and a Web UI.

> Note: This repository started from the upstream project `easy_proxies` and will continue to evolve.

## Features

- **Pool / Multi-Port / Hybrid** modes
- **Subscription links** (Base64 / Clash YAML / plain text) + manual refresh + optional auto-refresh
- **Web UI**: node status, latency probe, one-click export, settings, node CRUD, reload
- **GeoIP region filtering**: auto-detect node server location and build a selectable region list (single/multi select)
- **Auto health checks**: initial check on startup + periodic checks; automatic blacklist + manual release
- **Port preservation** across reloads (existing nodes keep their ports)

## Quick start (local)

1) Prepare config files:

```bash
cp config.example.yaml config.yaml
cp nodes.example nodes.txt
```

2) Edit `config.yaml` (minimal example):

```yaml
mode: pool
listener:
  address: 0.0.0.0
  port: 2323
  username: username
  password: password
management:
  enabled: true
  listen: 0.0.0.0:9090
  probe_target: www.apple.com:80

# Pick one (or mix):
# nodes_file: nodes.txt
subscriptions:
  - "https://example.com/subscribe"
```

3) Build & run:

```bash
go build -tags "with_utls with_quic with_grpc with_wireguard with_gvisor" -o easy-proxies ./cmd/easy_proxies
./easy-proxies --config config.yaml
```

4) Use the proxy:

- Pool entry: `http://username:password@127.0.0.1:2323`
- Web UI: `http://127.0.0.1:9090`

## Quick start (Docker)

This repo’s `docker-compose.yml` builds the image from source (so your code changes take effect).

```bash
cp config.example.yaml config.yaml
cp nodes.example nodes.txt
./start.sh
```

Update:

```bash
git pull --ff-only
./start.sh
```

## Web UI

Open `http://<management.listen>`:

- Dashboard: node status, latency, active connections, blacklist state
- Export: export available proxy entry links
- Node Management: add/edit/delete nodes, then reload to apply
- Settings:
  - external IP (used for exported links)
  - probe target
  - subscription links + refresh options
  - node filter (name / GeoIP / name_or_geo)

## Region filtering (GeoIP)

### Web UI (recommended)

1) Dashboard → **⚙️ Settings**  
2) **Region quick select** → click **Refresh list** to resolve GeoIP regions  
3) Select one or multiple regions (enable *Single* if you want single-select)  
4) Save settings → click **Reload** when prompted

The selected regions are synced to `node_filter.include` and the target is switched to `geo` automatically.

### Config file

```yaml
node_filter:
  target: geo         # name | geo | name_or_geo
  include: ["US", "Hong Kong", "洛杉矶"]
  exclude: []
  use_regex: false
```

Notes:

- GeoIP is **best-effort** and is **not persisted** into config files.
- GeoIP lookup uses `ipwho.is` by default. If the API is blocked in your network, regions may show as unknown/empty.

## Subscription refresh

- Add links in **Settings → Subscriptions**
- Click **Refresh** (or enable auto-refresh via `subscription_refresh`)

Important: refresh/reload restarts the sing-box core, which **disconnects existing connections**.

## Ports

- `2323`: pool entry (pool / hybrid)
- `24000+`: per-node entries (multi-port / hybrid)
- `9090`: Web UI (configurable via `management.listen`)

## Troubleshooting

- **Subscription fetch fails / shows all errors**: check system proxy / environment variables (`http_proxy`, `https_proxy`, `all_proxy`). A proxy loop can break fetch/resolve.
- **GeoIP unknown/empty**: the node host may not resolve to a public IP, or GeoIP API is unreachable; try again or use name-based filtering.
- **Web UI not reachable**: confirm `management.listen` is bound to `0.0.0.0` if you want remote access, and ensure ports are open.

## License

MIT License
