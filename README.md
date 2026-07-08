# Gatekeeper

Gatekeeper is a tiny reverse proxy for Kubernetes that **scales your workloads
to zero when they are idle and wakes them on the next request** - holding that
request until the backend is ready, so the caller sees a slightly slow first
response instead of an error. It can optionally **authenticate** every request
with a shared token.

It runs in two shapes:

- **Single-namespace** (`deploy/`): one Gatekeeper inside the namespace it
  manages, configured with a static `ROUTES_JSON`. The simple path.
- **Cluster mode** (`deploy/cluster/`): one 3-replica install (leader-elected,
  see [High availability](#high-availability-optional)) managing many
  namespaces, [discovering](#namespace-discovery-optional) them by label with
  routes in namespace annotations. Namespaces sleep and wake independently.

It ships as a single ~25 MB static binary on distroless, uses tens of MB of RAM,
starts instantly, and talks to the Kubernetes API with its own in-cluster
ServiceAccount. Everything is configured through environment variables.

## Why

Idle environments (per-branch/preview/staging/demo deployments, internal tools,
rarely-used services) burn CPU and memory around the clock. Gatekeeper sits in
front of them and:

- **Scales to zero** every selected Deployment and StatefulSet after an idle
  period, remembering each one's replica count (set `IDLE_TIMEOUT=0` to disable
  and run as a pure wake-on-request proxy).
- **Wakes on demand**: the next request restores those replicas - **in dependency
  order** when workloads declare a `DEPENDS_ON_ANNOTATION` (a workload's
  dependencies are scaled up and ready before it is, so an app never starts against
  a database that isn't up yet) - then waits for **every managed workload in the
  namespace** to become ready before proxying through (Knative activator style), so
  an app is never sent traffic before its dependencies are up. It keeps holding the
  request for as long as the pods are legitimately starting, and only gives up early
  if a pod is wedged in a state it won't recover from (bad/missing image, crash
  loop) - rather than failing on a fixed timer. Websocket upgrades and streaming
  responses are supported.
- **Optionally authenticates** requests with a shared token via a header or
  cookie, with an optional redirect to an external login.

## How it works

```
        +------------------------- namespace -------------------------+
request | Gatekeeper --proxy--> Service --> Pod(s)                    |
------> |    |                                                        |
        |    +- auth (optional): token in header/cookie, else 401     |
        |    +- idle > IDLE_TIMEOUT      -> scale targets to 0         |
        |    +- request while asleep     -> restore replicas, wait,    |
        |                                   then proxy                 |
        +-------------------------------------------------------------+
```

Gatekeeper routes by `Host` header using a table you provide (`ROUTES_JSON`). The
awake/asleep state is held in memory (run a single replica) and seeded from the
cluster at startup; each workload's pre-sleep replica count is saved on an
annotation, so a restart recovers cleanly.

## Quick start

1. Label the workloads you want managed (the default selector is opt-in):

   ```sh
   kubectl label deploy/my-app gatekeeper.dev/scale-to-zero=true
   ```

2. Edit `deploy/` (namespace, the `gatekeeper-routes` ConfigMap, and any env), then apply:

   ```sh
   kubectl apply -f deploy/
   ```

3. Point your Ingress / Gateway at the `gatekeeper` Service (port 80) for the
   hostnames in your routes table.

A complete, runnable example (a sample app + Gatekeeper + assertions for the full
auth/sleep/wake cycle) lives in `e2e/`. Run it against any local cluster:

```sh
./e2e/run.sh        # uses kube context "orbstack"; override with KUBE_CONTEXT
```

## Configuration

All configuration is via environment variables.

### Core

| Env | Default | Purpose |
|-----|---------|---------|
| `NAMESPACE` | *(see below)* | Default namespace for routes that don't name one. Inject via the downward API. |
| `ROUTES_JSON` | *(required)* | `{"host":{"service":"svc","port":80}, ...}` host -> upstream map. An entry may add `"namespace":"other-ns"` to route into (and manage) another namespace; entries without one use `NAMESPACE`. Required: every entry ends up with a namespace one way or the other. |
| `POD_NAMESPACE` | *(falls back to `NAMESPACE`; required if `NAMESPACE` is unset)* | Namespace Gatekeeper itself runs in (downward API). `SELF_NAME` is only excluded from scaling here - a workload merely named the same elsewhere is managed normally. |
| `PORT` | `8080` | Listen port. |
| `HEALTH_PATH` | `/healthz` | Unauthenticated health/probe path. |
| `NOT_FOUND_PAGE_FILE` | *(built-in page)* | Path to an HTML file served (with a 404) when no route matches the request's `Host`. Read once at startup; an unreadable path fails startup. Keep the page generic - it must not hint that hostnames are what an unauthenticated client is enumerating. |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error`. JSON logs to stdout. |

### Scale-to-zero

| Env | Default | Purpose |
|-----|---------|---------|
| `TARGET_SELECTOR` | `gatekeeper.dev/scale-to-zero=true` | Label selector for managed Deployments/StatefulSets. Empty selects every workload in the namespace. |
| `SELF_NAME` | `gatekeeper` | Workload name Gatekeeper never scales (itself). |
| `WAKE_REPLICAS_ANNOTATION` | `gatekeeper.dev/wake-replicas` | Annotation storing the pre-sleep replica count. |
| `DEPENDS_ON_ANNOTATION` | `gatekeeper.dev/depends-on` | Annotation (comma-separated workload names) declaring a workload's dependencies. Wake happens in dependency order: a workload's dependencies are scaled up and ready before it is. Deps naming an unmanaged workload are ignored; a cycle falls back to waking all at once. |
| `IDLE_TIMEOUT` | `30m` | Idle duration before scaling to zero (Go duration). Set to `0` to disable scale-to-zero: the namespace is never auto-slept, but requests still wake one that is already asleep. |
| `IDLE_CHECK_INTERVAL` | `30s` | How often idleness is checked. Must be > 0 while scale-to-zero is enabled. |
| `WAKE_TIMEOUT` | `5m` | Backstop for how long a request is held while the namespace wakes (all managed workloads become ready) before giving up (503 + `Retry-After`). Generous so slow-but-healthy starts (large image pulls, cold nodes) aren't cut off; a wake that hits a wedged pod fails fast well before this. |

### Two settings that must line up

Most "it deployed but nothing works" cases come from one of these drifting out of sync:

- **`HEALTH_PATH` must equal your readiness/liveness probe path.** The probe hits this
  path on the pod IP; if Gatekeeper doesn't recognize it as the health path the request
  falls through to host-routing, 404s (`no route for host: <pod-ip>:8080` in the logs),
  and the pod never goes Ready - so the Service has no endpoints.
- **`TARGET_SELECTOR` must match the labels on the workloads you want scaled** (and
  `SELF_NAME` must be Gatekeeper's own Deployment name so it never scales itself). If the
  selector matches nothing, idle scaling silently does nothing.

### High availability (optional)

With `LEADER_ELECTION=true`, run several replicas: they elect a leader through a
Lease, and only the leader serves traffic, seeds power state, and runs the idle
loop - the rest are warm standbys. Traffic is steered by a **pod label**
(`gatekeeper.dev/role=leader`) that the leader sets on itself and the Service
selects on; readiness stays uniform across replicas on purpose, since a
Deployment with permanently-unready standbys could never complete a rollout.
If the leader dies, a standby acquires the Lease (up to ~15s), re-derives all
sleep/wake state from the cluster, and only then labels itself. Any replica
that isn't the seeded leader answers proxied requests with 503 + `Retry-After`
(probes and the auth callback are served everywhere), so a pod restarted with
a stale leader label can never serve off wrong state. Don't add a
PodDisruptionBudget: with one traffic-carrying pod it can only do nothing or
block node drains - failover *is* the disruption tolerance.

`deploy/cluster/` contains a complete cluster-mode install (3 replicas, leader
Service, ClusterRole + leader Role).

| Env | Default | Purpose |
|-----|---------|---------|
| `LEADER_ELECTION` | `false` | Enable active-passive HA. Requires `POD_NAME` and `POD_NAMESPACE` (downward API) and the `leases` + `pods get,patch` Role from `deploy/cluster/rbac.yaml`. |
| `POD_NAME` | *(required with election)* | This pod's name: the election identity and the pod that gets the leader label. |
| `LEASE_NAME` | `gatekeeper` | Lease object name, created in `POD_NAMESPACE`. |
| `READY_PATH` | `/readyz` | Readiness probe path; must differ from `HEALTH_PATH` (liveness stays unconditional). |

### Namespace discovery (optional)

Instead of a static `ROUTES_JSON`, set `NAMESPACE_SELECTOR` and Gatekeeper
**discovers** its namespaces: it watches Namespaces matching the selector and
reads each one's routes from an annotation. Create a labeled+annotated
namespace and it is routable within milliseconds; delete or unlabel it and its
routes vanish with it. There is no central routes object to keep in sync (and
so no concurrent writers fighting over one).

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: preview-pr-12
  labels:
    gatekeeper.dev/managed: "true"          # must match NAMESPACE_SELECTOR
  annotations:
    gatekeeper.dev/routes: |                # same shape as ROUTES_JSON values
      { "app-pr12.example.com": { "service": "web", "port": 3000 } }
    gatekeeper.dev/idle-timeout: "45m"      # optional per-namespace override
```

| Env | Default | Purpose |
|-----|---------|---------|
| `NAMESPACE_SELECTOR` | *(empty = static mode)* | Label selector for managed namespaces. Mutually exclusive with `ROUTES_JSON`. |
| `ROUTES_ANNOTATION` | `gatekeeper.dev/routes` | Annotation holding a namespace's routes JSON. |
| `IDLE_TIMEOUT_ANNOTATION` | `gatekeeper.dev/idle-timeout` | Per-namespace idle override (Go duration; `0` disables auto-sleep for that namespace). |

Rules worth knowing:

- An annotation's routes always target **its own namespace** - a `namespace`
  field in the annotation is rejected, so annotating your namespace can never
  steer traffic into someone else's.
- A labeled namespace with a missing/malformed annotation is **skipped with a
  warning and a Kubernetes Event** on the Namespace (`kubectl get events -n
  default --field-selector involvedObject.name=<ns>`); other namespaces are
  unaffected.
- If two namespaces claim the same host, the **oldest namespace wins**,
  deterministically; the loser gets an Event.
- `GET /_gatekeeper/routes` returns the live routing table and each
  namespace's awake/asleep state - the discovery-mode replacement for
  eyeballing a routes ConfigMap. It requires authentication and therefore
  **only exists while `AUTH_TOKEN` is set**: the table enumerates every
  hostname, which may be your deployment's only secret (e.g. public previews
  behind unguessable hostnames). With auth off the path is not intercepted at
  all; inspect the namespace labels/annotations instead.
- RBAC: add cluster-wide `namespaces get,list,watch` and `events create,patch`
  (included in `deploy/cluster/rbac.yaml`).

### Authentication (optional)

Authentication is **off** unless `AUTH_TOKEN` is set - Gatekeeper is then a plain
scale-to-zero proxy. When set, every request except the health and callback paths
must carry the token.

| Env | Default | Purpose |
|-----|---------|---------|
| `AUTH_TOKEN` | *(empty = auth off)* | Shared secret required on every request. |
| `AUTH_HEADER` | `X-Gatekeeper-Token` | Header carrying the token. |
| `AUTH_COOKIE` | `gatekeeper_session` | Cookie carrying the token. |
| `LOGIN_URL` | *(empty)* | If set, unauthenticated browsers are redirected here as `?redirect=<original-url>`; if empty, they get 401. |
| `AUTH_CALLBACK_PATH` | `/_gatekeeper/auth` | Page that reads `?token=&next=`, sets the cookie, and redirects to `next`. |
| `COOKIE_DOMAIN` | *(empty)* | Scope the cookie to `.<domain>` (shared across subdomains); empty = host-only. |

**Auth modes:**

- **No auth** - leave `AUTH_TOKEN` unset.
- **Static token** - set `AUTH_TOKEN` (and optionally `AUTH_HEADER`). Callers send
  the header; missing/invalid gets 401. Good for service-to-service traffic or an
  upstream gateway that injects the header.
- **Browser login** - also set `LOGIN_URL` (and usually `COOKIE_DOMAIN`).
  Unauthenticated browsers are sent to your login, which authenticates the user and
  then redirects to `https://<host>{AUTH_CALLBACK_PATH}?token=<token>&next=<original>`
  to drop the cookie. Subsequent requests carry the cookie.

## RBAC

**Cluster mode** needs the ClusterRole (workloads + namespaces + events across
managed namespaces) and the leader Role (leases, pod labeling) from
`deploy/cluster/rbac.yaml`; the namespace-annotation design keeps the
cluster-wide reads to Namespaces only. **Single-namespace mode** runs as its
own ServiceAccount with just a namespaced Role:

```yaml
rules:
  - apiGroups: ["apps"]
    resources: ["deployments", "statefulsets"]
    verbs: ["get", "list", "watch", "patch"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["list"]
```

`patch` on the workloads sets `spec.replicas` and the wake annotation in one merge
patch; their `status.readyReplicas` is polled to know when the namespace is ready.
`pods` are listed on wake to fail fast when a managed pod is wedged (bad image,
crash loop) instead of waiting out `WAKE_TIMEOUT`. `deploy/` contains the full set
(ServiceAccount, Role, RoleBinding).

> **API-server egress (CNIs that enforce NetworkPolicy: AWS VPC CNI `aws-node`, Calico,
> Cilium, ...).** Under a default-deny egress policy, Gatekeeper's scale calls to the
> Kubernetes API server are dropped - you'll see `dial tcp <apiserver-ip>:443: i/o timeout` -
> until you allow it. Apply `deploy/networkpolicy-apiserver-egress.yaml`, which permits
> egress to `0.0.0.0/0:443,6443` for the Gatekeeper pod only. Two subtleties bite here:
>
> - A broad egress policy that `ipBlock`s `0.0.0.0/0` with an `except` for RFC1918 ranges
>   still blocks the API server, since its ClusterIP/ENI lives in those ranges.
> - Worse: if that `except`-bearing policy **also selects the Gatekeeper pod**, the AWS VPC
>   CNI agent enforces each `except` as a longest-prefix-match **deny** that shadows this
>   policy's `0.0.0.0/0` allow (the `/12` deny beats the `/0` allow). Adding the allow is then
>   not enough - keep the `except`-bearing policy off the Gatekeeper pod (e.g.
>   `podSelector: { matchExpressions: [{ key: app, operator: NotIn, values: [gatekeeper] }] }`)
>   or make the API-server allow more specific than the `except` (e.g. the service CIDR `/16`).
>
> On **Cilium**, a plain `ipBlock` may not match the API-server identity - use a
> `CiliumNetworkPolicy` with `toEntities: [kube-apiserver]` instead.

## Develop

```sh
make all              # gofmt check + vet + test + build
make docker           # build the container image
./e2e/run.sh          # e2e: single-namespace mode (static routes, auth, sleep/wake)
./e2e/run-cluster.sh  # e2e: cluster mode (discovery, leader failover, isolation)
```

Both e2e suites target a local cluster (OrbStack / kind / docker-desktop) and
refuse anything else. Migrating a fleet of per-namespace installs to cluster
mode? See [docs/migration-previewkit.md](docs/migration-previewkit.md).

Go 1.26+. The module is `github.com/autonoma-ai/gatekeeper`.

## License

MIT - see [LICENSE](LICENSE).
