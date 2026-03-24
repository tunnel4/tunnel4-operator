# tunnel4-operator

Kubernetes operator for [tun4.dev](https://tun4.dev) — hybrid dev environment platform.

Manages the lifecycle of isolated developer namespaces in a shared K8s cluster. Each developer gets a dedicated namespace per git branch with stub pods that proxy traffic through the WireGuard tunnel to the developer's local machine.

## Overview

Part of the **tun4.dev** platform. When a developer runs `devenv up`, the local agent triggers creation of a `DevEnvironment` CR. This operator:

1. Creates an isolated namespace (`dev-<developer>-<branch>`)
2. Deploys stub pods for each intercepted service
3. Monitors heartbeat and scales to zero after 30 min idle
4. Cleans up all resources on deletion

Traffic flow: `cluster service → stub pod → WireGuard tunnel → local machine → back`

## Custom Resource

```yaml
apiVersion: devenv.tunnel4.dev/v1
kind: DevEnvironment
metadata:
  name: devenvironment-sample
spec:
  developer: alice
  branch: feature-payments
  developerTunnelIP: 10.8.0.5   # assigned by WireGuard
  intercepts:
    - service: payments-svc
      port: 8080
    - service: notifications-svc
      port: 9090
  ttl: 8h                        # optional hard TTL
```

### Status Phases

| Phase | Description |
|-------|-------------|
| `Provisioning` | Creating namespace and stub pods |
| `Ready` | Environment running, monitoring heartbeat |
| `Sleeping` | Idle >30 min — pods scaled to 0 |
| `Terminating` | Deletion in progress |
| `Failed` | Error state |

## Getting Started

### Prerequisites

- Go v1.24+
- Docker v17.03+
- kubectl v1.11.3+
- Kubernetes v1.11.3+ cluster (dev server: `187.124.157.227`)

### Deploy

```sh
# Build and push image
make docker-build docker-push IMG=<registry>/tunnel4-operator:tag

# Install CRDs
make install

# Deploy operator
make deploy IMG=<registry>/tunnel4-operator:tag
```

### Try it

```sh
kubectl apply -k config/samples/
kubectl get devenvironment
kubectl get ns -l tunnel4.dev/managed-by=operator
```

### Uninstall

```sh
kubectl delete -k config/samples/
make uninstall
make undeploy
```

## Network Layout

| Range | Purpose |
|-------|---------|
| `10.8.0.0/24` | WireGuard tunnel (local agents) |
| `10.42.0.0/16` | Pod CIDR |
| `10.43.0.0/16` | Service CIDR |
| `10.43.0.10` | CoreDNS |

## Architecture

Each `DevEnvironment` owns:
- **Namespace** — `dev-<developer>-<branch>`, labeled with `tunnel4.dev/*`
- **ResourceQuota** — limits CPU/memory per environment
- **NetworkPolicy** — isolates namespace traffic
- **Stub Deployments + Services** — one per intercepted service, routes to `developerTunnelIP`
- **TLS Certificates** — via cert-manager (optional)

The operator uses a finalizer (`tunnel4.dev/finalizer`) to ensure namespace cleanup on deletion.

## Development

```sh
make run          # run controller locally against current kubeconfig
make test         # run unit + integration tests
make generate     # regenerate CRD manifests after API changes
make help         # all available targets
```

## License

Copyright 2026. Licensed under the [Apache License 2.0](http://www.apache.org/licenses/LICENSE-2.0).
