# consul-sync

A lightweight Go controller that watches HashiCorp Consul for services tagged with a configurable label and syncs them into Kubernetes as headless Services + EndpointSlices. This enables Gateway API (e.g., Envoy Gateway) to route traffic to services running on external hosts like Docker VMs.

## How It Works

```
Docker Host(s)                        Kubernetes Cluster (network namespace)
┌──────────────────────┐             ┌─────────────────────────────────────┐
│ Registrator          │──register──▶│ Consul (app-template HelmRelease)  │
│   watches Docker     │             │   LoadBalancer IP on 10.69.0.0/24  │
│   socket, registers  │             │   Persistent storage via Longhorn  │
│   containers w/      │             │                                    │
│   SERVICE_NAME env   │             │ consul-sync controller             │
│                      │             │   watches Consul (ClusterIP)       │
│ Docker containers    │             │   creates Services+EndpointSlices  │
│   ├─ plex            │             │                                    │
│   ├─ homeassistant   │◀──traffic──│ Envoy Gateway                      │
│   └─ ...             │             │   HTTPRoutes → synced Services     │
└──────────────────────┘             └─────────────────────────────────────┘
```

1. Polls Consul `/v1/catalog/services?tag=kubernetes` using blocking queries (long-poll, near-instant updates)
2. For each tagged service, fetches healthy instances via `/v1/health/service/<name>?passing=true`
3. Creates/updates a headless `Service` (clusterIP: None) and an `EndpointSlice` with the instance IPs
4. Auto-generates `HTTPRoute` resources based on Consul service tags (`internal`/`external`) so services are immediately routable through Envoy Gateway
5. Cleans up orphaned Kubernetes resources (Services, EndpointSlices, HTTPRoutes) when services deregister from Consul
6. Performs a full safety resync every 5 minutes as a fallback

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
| `ENABLE_HTTPROUTES` | No | `true` | Enable auto-generation of HTTPRoute resources |
| `DOMAIN_SUFFIX` | No | `k8s.alexieff.io` | Hostname pattern: `<service>.<suffix>` |
| `INTERNAL_GATEWAY` | No | `envoy-internal` | Gateway resource name for internal routes |
| `EXTERNAL_GATEWAY` | No | `envoy-external` | Gateway resource name for external routes |
| `GATEWAY_NAMESPACE` | No | (uses `TARGET_NAMESPACE`) | Namespace of both Gateway resources |
| `GATEWAY_LISTENER` | No | `https` | Listener section name on the Gateway |
| `INTERNAL_TAG` | No | `internal` | Consul tag that triggers an internal gateway route |
| `EXTERNAL_TAG` | No | `external` | Consul tag that triggers an external gateway route |

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
| `consul_sync_httproutes_total` | Gauge | Number of currently synced HTTPRoute resources |

## Project Structure

```
consul-sync/
├── cmd/consul-sync/main.go           # Entrypoint, config, signal handling
├── internal/
│   ├── consul/
│   │   ├── types.go                   # ServiceState, ServiceInstance
│   │   └── watcher.go                 # Consul blocking-query watcher
│   ├── kubernetes/
│   │   └── syncer.go                  # Service + EndpointSlice + HTTPRoute reconciliation
│   ├── reconciler/
│   │   └── reconciler.go             # Orchestrates watcher → syncer loop
│   ├── metrics/
│   │   └── metrics.go                 # Prometheus counters/gauges
│   └── health/
│       └── health.go                  # /healthz, /readyz, /metrics server
├── consul-server/
│   └── docker-compose.yaml            # Registrator (points at Consul in K8s)
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
export CONSUL_ADDR=http://<consul-lb-ip>:8500
export CONSUL_TOKEN=your-token-here
export KUBECONFIG=~/.kube/config

go run ./cmd/consul-sync
```

## Consul Server

Consul runs in Kubernetes (deployed via Flux in the `network` namespace using the bjw-s app-template). The cluster repo contains the deployment at `kubernetes/apps/network/consul/`.

ACL is enabled with `initial_management` token sourced from 1Password via ExternalSecret. After deploying Consul, run the ACL setup script in the cluster repo to create agent tokens:

```bash
# From the cluster repo
./scripts/setup-consul-acl.sh <management-token>
```

### Registrator Setup (Docker Hosts)

`consul-server/docker-compose.yaml` runs [Registrator](https://github.com/gliderlabs/registrator) on Docker hosts, pointing at the Consul LoadBalancer IP in Kubernetes.

```bash
# Get the Consul LoadBalancer IP
kubectl -n network get svc consul -o jsonpath='{.status.loadBalancer.ingress[0].ip}'

# Start Registrator
CONSUL_HTTP_ADDR=<consul-lb-ip> CONSUL_TOKEN=<registrator-token> docker compose up -d
```

Registrator runs with `-explicit=true`, so only containers with `SERVICE_NAME` set are registered. The `kubernetes` tag is added automatically.

### Registering Docker Services

Add these environment variables to your application containers:

```yaml
services:
  plex:
    image: plexinc/pms-docker
    ports:
      - "32400:32400"
    environment:
      SERVICE_NAME: plex
      SERVICE_TAGS: internal           # creates HTTPRoute for envoy-internal
      SERVICE_32400_CHECK_HTTP: /web
      SERVICE_32400_CHECK_INTERVAL: 30s
      SERVICE_32400_CHECK_TIMEOUT: 5s
```

| Variable | Description |
|---|---|
| `SERVICE_NAME` | Consul service name (required for registration) |
| `SERVICE_<port>_CHECK_HTTP` | HTTP health check path |
| `SERVICE_<port>_CHECK_TCP` | TCP health check (alternative to HTTP) |
| `SERVICE_<port>_CHECK_INTERVAL` | Health check interval |
| `SERVICE_<port>_CHECK_TIMEOUT` | Health check timeout |
| `SERVICE_TAGS` | Additional comma-separated tags (use `internal`/`external` for HTTPRoute generation) |

When a container stops, Registrator automatically deregisters it from Consul.

## Kubernetes Deployment

consul-sync is deployed via Flux in the `network` namespace. The manifests live in the cluster repo at `kubernetes/apps/network/consul-sync/`.

### RBAC

The controller requires a ClusterRole with CRUD access to:
- `v1/Services`
- `discovery.k8s.io/v1/EndpointSlices`
- `gateway.networking.k8s.io/v1/HTTPRoutes` (verbs: `get`, `list`, `patch`, `delete`)

### HTTPRoute Auto-Generation

consul-sync automatically creates HTTPRoute resources based on Consul service tags, so services are immediately routable through Envoy Gateway without manual HTTPRoute creation.

**Tag behavior:**
- `internal` tag → HTTPRoute targeting `envoy-internal` gateway
- `external` tag → HTTPRoute targeting `envoy-external` gateway
- Both tags → two HTTPRoutes (one per gateway)
- Neither tag → no HTTPRoute (Service + EndpointSlice still created)

Tags are set via Registrator's `SERVICE_TAGS` environment variable (see [Registering Docker Services](#registering-docker-services)).

**Generated HTTPRoute example:**

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: plex-envoy-internal
  namespace: network
  labels:
    app.kubernetes.io/managed-by: consul-sync
    app.kubernetes.io/name: plex
spec:
  parentRefs:
    - name: envoy-internal
      namespace: network
      sectionName: https
  hostnames:
    - plex.k8s.alexieff.io
  rules:
    - backendRefs:
        - name: plex
          port: 32400
```

Orphaned HTTPRoutes are automatically cleaned up when the corresponding Consul service tags are removed or the service is deregistered.

To disable auto-generation and manage HTTPRoutes manually, set `ENABLE_HTTPROUTES=false`.

## Verifying

```bash
# Check synced services
kubectl get svc -n network -l app.kubernetes.io/managed-by=consul-sync

# Check endpoints
kubectl get endpointslice -n network -l endpointslice.kubernetes.io/managed-by=consul-sync

# Check auto-generated HTTPRoutes
kubectl get httproute -n network -l app.kubernetes.io/managed-by=consul-sync

# Check controller logs
kubectl logs -n network deploy/consul-sync
```
