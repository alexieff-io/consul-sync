# consul-sync

A lightweight Go controller that watches HashiCorp Consul for services tagged with a configurable label and syncs them into Kubernetes as headless Services + EndpointSlices. This enables Gateway API (e.g., Envoy Gateway) to route traffic to services running on external hosts like Docker VMs.

## How It Works

```
Docker Host (different subnet)         Kubernetes Cluster
┌─────────────────────────┐           ┌──────────────────────────────────┐
│ Consul Server           │           │  consul-sync controller          │
│   ├─ plex (tagged       │◄──watch──│    │                             │
│   │   "kubernetes")     │           │    ├─ Creates Service "plex"     │
│   ├─ homeassistant      │           │    └─ Creates EndpointSlice      │
│   └─ ...                │           │         with Docker host IPs     │
│                         │           │                                  │
│ Docker containers       │           │  Envoy Gateway                   │
│   ├─ plex:32400     ◄───┼───────────┤    ├─ HTTPRoute → Service "plex" │
│   ├─ hass:8123      ◄───┼───────────┤    └─ TLS termination + auth    │
│   └─ ...                │           │                                  │
└─────────────────────────┘           └──────────────────────────────────┘
```

1. Polls Consul `/v1/catalog/services?tag=kubernetes` using blocking queries (long-poll, near-instant updates)
2. For each tagged service, fetches healthy instances via `/v1/health/service/<name>?passing=true`
3. Creates/updates a headless `Service` (clusterIP: None) and an `EndpointSlice` with the instance IPs
4. Cleans up orphaned Kubernetes resources when services deregister from Consul
5. Performs a full safety resync every 5 minutes as a fallback

All managed resources are labeled `app.kubernetes.io/managed-by: consul-sync`.

## Configuration

All configuration is via environment variables:

| Variable | Required | Default | Description |
|---|---|---|---|
| `CONSUL_ADDR` | Yes | — | Consul HTTP address (e.g., `http://10.0.10.100:8500`) |
| `CONSUL_TOKEN` | No | — | Consul ACL token (read-only access to services/nodes) |
| `CONSUL_TAG` | No | `kubernetes` | Only sync services with this tag |
| `TARGET_NAMESPACE` | No | `network` | Kubernetes namespace for created resources |
| `METRICS_ADDR` | No | `:8080` | Listen address for health checks and Prometheus metrics |
| `RESYNC_INTERVAL` | No | `5m` | Interval for full resync from Consul |

## Endpoints

| Path | Description |
|---|---|
| `GET /healthz` | Liveness probe — always returns 200 |
| `GET /readyz` | Readiness probe — returns 200 after first successful sync, 503 before |
| `GET /metrics` | Prometheus metrics |

## Metrics

| Metric | Type | Description |
|---|---|---|
| `consul_sync_services_total` | Gauge | Number of currently synced services |
| `consul_sync_endpoints_total` | Gauge | Total endpoints across all synced services |
| `consul_sync_reconcile_total` | Counter | Reconciliations performed (labels: `status=success\|error`) |
| `consul_sync_consul_errors_total` | Counter | Errors communicating with Consul |
| `consul_sync_kubernetes_errors_total` | Counter | Errors communicating with the Kubernetes API |

## Project Structure

```
consul-sync/
├── cmd/consul-sync/main.go           # Entrypoint, config, signal handling
├── internal/
│   ├── consul/
│   │   ├── types.go                   # ServiceState, ServiceInstance
│   │   └── watcher.go                 # Consul blocking-query watcher
│   ├── kubernetes/
│   │   └── syncer.go                  # Service + EndpointSlice reconciliation
│   ├── reconciler/
│   │   └── reconciler.go             # Orchestrates watcher → syncer loop
│   ├── metrics/
│   │   └── metrics.go                 # Prometheus counters/gauges
│   └── health/
│       └── health.go                  # /healthz, /readyz, /metrics server
├── consul-server/                     # Docker host Consul setup
│   ├── docker-compose.yaml
│   ├── consul.d/server.json
│   ├── consul.d/plex.json            # Example service registration
│   ├── consul.d/homeassistant.json   # Example service registration
│   └── setup-acl.sh                  # ACL bootstrap script
├── Dockerfile
├── go.mod
└── go.sum
```

## Building

```bash
# Build locally
go build -o consul-sync ./cmd/consul-sync

# Build container image
docker build -t ghcr.io/alexieff-io/consul-sync:latest .
```

## Local Development

```bash
export CONSUL_ADDR=http://10.0.10.100:8500
export CONSUL_TOKEN=your-token-here
export KUBECONFIG=~/.kube/config

go run ./cmd/consul-sync
```

## Consul Server Setup

See `consul-server/` for a ready-to-use Docker Compose setup.

```bash
cd consul-server

# Start Consul
docker compose up -d

# Bootstrap ACL and create tokens
./setup-acl.sh
```

The setup script creates:
- A **master token** (save securely)
- A **consul-sync token** (read-only, store in 1Password as `consul-acl-token`)
- An **agent token** (for service registration)

### Registering Services

Add JSON files to `consul-server/consul.d/` and reload Consul:

```json
{
  "service": {
    "name": "myapp",
    "tags": ["kubernetes"],
    "port": 8080,
    "address": "10.0.10.100",
    "check": {
      "http": "http://10.0.10.100:8080/health",
      "interval": "30s",
      "timeout": "5s"
    }
  }
}
```

```bash
docker exec consul consul reload
```

## Kubernetes Deployment

consul-sync is deployed via Flux in the `network` namespace. The manifests live in the cluster repo at `kubernetes/apps/network/consul-sync/`.

### RBAC

The controller requires a ClusterRole with CRUD access to:
- `v1/Services`
- `discovery.k8s.io/v1/EndpointSlices`

### Routing to Synced Services

consul-sync creates headless Services — define HTTPRoutes separately to expose them through Envoy Gateway:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: myapp
  namespace: network
spec:
  parentRefs:
    - name: envoy-internal
      namespace: network
      sectionName: https
  hostnames:
    - "myapp.k8s.alexieff.io"
  rules:
    - backendRefs:
        - name: myapp   # Created by consul-sync
          port: 8080
```

## Verifying

```bash
# Check synced services
kubectl get svc -n network -l app.kubernetes.io/managed-by=consul-sync

# Check endpoints
kubectl get endpointslice -n network -l endpointslice.kubernetes.io/managed-by=consul-sync

# Check controller logs
kubectl logs -n network deploy/consul-sync
```
