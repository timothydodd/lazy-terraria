# lazy terraria server

> TShock Terraria server on k3s, with a wake-on-connect proxy that scales the server to zero when nobody's playing.

[![Build proxy image](https://github.com/timothydodd/terraria/actions/workflows/docker-publish-proxy.yml/badge.svg)](https://github.com/timothydodd/terraria/actions/workflows/docker-publish-proxy.yml)
[![Docker Image](https://img.shields.io/docker/v/timdoddcool/terraria-proxy?label=proxy%20image&logo=docker)](https://hub.docker.com/r/timdoddcool/terraria-proxy)
[![TShock](https://img.shields.io/badge/server-TShock-blue)](https://github.com/Pryaxis/TShock)
[![k3s](https://img.shields.io/badge/cluster-k3s-orange)](https://k3s.io)

The Terraria dedicated server busy-loops on the main thread and pegs a full
CPU core whether anyone's online or not. This repo wraps it with a tiny Go
proxy that listens on the public LoadBalancer, scales the StatefulSet on the
first TCP connection, and scales back to zero after an idle window.

---

## Contents

- [Architecture](#architecture)
- [Quick start](#quick-start)
- [Repository layout](#repository-layout)
- [Configuration](#configuration)
- [Wake-on-connect proxy](#wake-on-connect-proxy)
- [Operations](#operations)
- [CI / Docker Hub](#ci--docker-hub)
- [Upgrading](#upgrading)

---

## Architecture

```
                 ┌──────────────────────────────────────┐
                 │              k3s cluster             │
                 │                                      │
  Terraria       │  ┌───────────────┐   scale 0↔1       │
  client  ─────► │  │ terraria-proxy├──────────┐        │
  :7777   LB     │  │  (Deployment) │          ▼        │
                 │  └──────┬────────┘   ┌──────────────┐│
                 │         │ splice TCP │  terraria    ││
                 │         └───────────►│ (StatefulSet)││
                 │       terraria-backend│   TShock    ││
                 │           ClusterIP   └──────────────┘│
                 └──────────────────────────────────────┘
```

The proxy uses the Kubernetes `scale` subresource on the StatefulSet — no
custom CRDs, no operators.

## Quick start

```sh
# 1. Apply everything
kubectl apply -f k3s/

# 2. Watch first-boot world generation
kubectl -n terraria logs -f statefulset/terraria

# 3. Grab the external IP
kubectl -n terraria get svc terraria
```

Connect from the Terraria client to the LB IP on port `7777`.

> **First connect after idle takes ~10–30s.** The proxy is scaling the pod up
> and waiting for TShock to bind the port. The client's connect timeout is
> generous; if it gives up, just retry.

## Repository layout

| Path | Purpose |
|------|---------|
| `k3s/` | k3s manifests: namespace, PVCs, ConfigMap, StatefulSet (TShock), Deployment (proxy), Services, RBAC. |
| `proxy/` | Go source + Dockerfile for the wake-on-connect proxy. |
| `.github/workflows/docker-publish-proxy.yml` | CI that builds and pushes the proxy image to Docker Hub. |

## Configuration

### Server (TShock)

- **World autocreation** — `k3s/configmap.yaml` (`serverconfig.txt`). Only used on first boot to seed the world; further changes belong in TShock's runtime config.
- **TShock runtime** — `/tshock/config.json` on the `tshock-config` PVC. Generated on first boot:
  ```sh
  kubectl -n terraria exec -it statefulset/terraria -- sh
  # vi /tshock/config.json
  ```
- **Server-side characters / starting inventory** — `sscconfig.json` in the ConfigMap (seeded to the PVC on first boot).
- **Plugins** — drop `.dll` files into the `tshock-plugins` PVC (mounted at `/plugins`) and roll the pod.
- **World files** — live on the `terraria-worlds` PVC (k3s `local-path`).

### Proxy

Tuned via env vars on `deployment/terraria-proxy`:

| Variable | Default | Description |
|----------|---------|-------------|
| `LISTEN_ADDR` | `:7777` | Address the proxy listens on. |
| `BACKEND_ADDR` | `terraria-backend.terraria.svc.cluster.local:7777` | Cluster-internal address it dials. |
| `NAMESPACE` | `terraria` | Namespace of the target StatefulSet. |
| `STATEFULSET` | `terraria` | StatefulSet to scale. |
| `IDLE_TIMEOUT` | `10m` | Idle window before scaling to 0. |
| `WAKE_TIMEOUT` | `120s` | How long to wait for the backend to accept TCP after scaling up. |
| `CHECK_INTERVAL` | `30s` | How often the idle watcher runs. |

## Wake-on-connect proxy

The public `terraria` LoadBalancer selects `app: terraria-proxy`. When a TCP
connection arrives, the proxy:

1. `PATCH`es `statefulset/terraria` to `replicas: 1` if it's at 0.
2. Polls TCP `terraria-backend:7777` until it accepts (or `WAKE_TIMEOUT` expires).
3. Splices the client connection through.

When the active connection count hits 0 and stays there for `IDLE_TIMEOUT`,
the watcher scales back to 0.

<details>
<summary>What the proxy does <strong>not</strong> do</summary>

- It doesn't parse the Terraria protocol — any TCP connect (port scan, LB
  probe, accidental `nc`) will wake the server.
- It doesn't gate by password or version — that's TShock's job.
- It doesn't do graceful drain at scale-down. The pod gets a normal SIGTERM
  with `terminationGracePeriodSeconds: 30`, which TShock handles cleanly.
  Bump the grace period if your world is large.

</details>

### Build the proxy image manually

```sh
cd proxy
docker build -t docker.io/timdoddcool/terraria-proxy:latest .
docker push docker.io/timdoddcool/terraria-proxy:latest
```

Or for a single-node k3s install, import directly into the local containerd:

```sh
docker save timdoddcool/terraria-proxy:latest -o proxy.tar
sudo k3s ctr images import proxy.tar
```

## Operations

### Console

```sh
kubectl -n terraria attach -it statefulset/terraria
# detach: Ctrl-P Ctrl-Q
```

Useful console commands: `save`, `say`, `exit`, plus all TShock commands.

### First-boot admin setup

```sh
kubectl -n terraria attach -it statefulset/terraria
# look for: "SetupCode: 1234567"
# then in-game: /setup 1234567
# then in-game: /user add <name> <password> superadmin
# then in-game: /setup   (disables the setup code)
```

### Disable the proxy temporarily

```sh
kubectl -n terraria scale deployment/terraria-proxy --replicas=0
kubectl -n terraria scale statefulset/terraria --replicas=1
kubectl -n terraria patch svc terraria --type=merge -p \
  '{"spec":{"selector":{"app":"terraria"}}}'
```

### Force-flush before destructive ops

```sh
# from console
save
exit
```

## CI / Docker Hub

`.github/workflows/docker-publish-proxy.yml` builds `proxy/` and publishes to
Docker Hub on every push to `main` (when `proxy/**` changes) and on `v*` tags.

Setup in GitHub:

1. **Settings → Secrets and variables → Actions → Variables** —
   `DOCKERHUB_USERNAME` = `timdoddcool`.
2. **Settings → Secrets and variables → Actions → Secrets** —
   `DOCKERHUB_TOKEN` from <https://hub.docker.com/settings/security>.
3. Push to `main`.

Image tags published:

| Trigger | Tags |
|---------|------|
| Push to `main` | `latest`, `sha-<short>` |
| Tag `v1.2.3` | `1.2.3`, `1.2`, `sha-<short>` |

## Upgrading

### TShock

Bump `image: ghcr.io/pryaxis/tshock:stable` to a pinned tag in
`k3s/statefulset.yaml`, then:

```sh
kubectl apply -f k3s/
kubectl -n terraria rollout restart statefulset/terraria
```

### Proxy

Edit `proxy/`, push to `main`, the CI publishes `:latest`. Then:

```sh
kubectl -n terraria rollout restart deployment/terraria-proxy
```

## License

MIT.
