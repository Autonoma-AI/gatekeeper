# Multi-namespace + HA implementation plan

Status: draft for review — 2026-07-03

Gatekeeper today is one controller per namespace: previewkit stamps a full
gatekeeper (Deployment, SA, Role, routes ConfigMap) into every `preview-*`
namespace. This plan consolidates that into a **single gatekeeper deployment
that observes many namespaces**, discovers them dynamically, and runs
**active-passive across 3 replicas**.

Every phase keeps the current single-namespace mode working unchanged, so the
existing per-namespace deployments stay valid until previewkit cuts over.

## Decisions

- **Multi-namespace**: one gatekeeper manages N namespaces; all per-namespace
  logic (sleep/wake/readiness/idle) is instantiated per namespace.
- **Config source: namespace discovery.** Namespaces opt in via a label;
  routes live in an annotation on the Namespace object. No central routes
  ConfigMap (avoids previewkit read-modify-write races and stale-route GC).
  Watched with a single shared informer; the static `ROUTES_JSON` mode remains
  supported for single-namespace and e2e use.
- **HA: active-passive, 3 replicas, Lease-based leader election**
  (client-go `leaderelection`, defaults: lease 15s / renew 10s / retry 2s).
  All sleep/wake/activity state stays in-memory on the leader; failover
  re-seeds from the cluster exactly like today's restart path.
- **Traffic steering: leader-labeled Service selector** — the leader labels
  its own pod `gatekeeper.dev/role=leader` and the Service selects on it.
  *(Revision of the earlier "leader-gated readiness" idea, which is unusable:
  a Deployment where 2 of 3 pods are permanently NotReady never completes a
  rollout, trips `progressDeadlineSeconds` into a failed state, and deadlocks
  PDB-aware drains. Readiness must stay uniform across replicas; leadership is
  expressed as a label instead.)*
- Consequence of the above: **no PodDisruptionBudget.** With one meaningful
  pod, any PDB either does nothing or blocks drains. Disruption tolerance *is*
  the failover.

## Target architecture

```
                        system namespace
                 +--------------------------------------+
 Ingress ------> | Service (selector: role=leader)      |
 (all preview    |   gatekeeper x3  [leader | standby | standby]
  hosts)         +-----|--------------------------------+
                       | informer: Namespaces (label-filtered)
                       | patch: Deployments/StatefulSets in preview-*
        +--------------+---------------+
        v                              v
  preview-a (labeled ns)         preview-b (labeled ns)
  routes annotation              routes annotation
  web / api / db workloads       web / api / db workloads
```

Request path per request: resolve `Host` → (namespace, service, port) → auth →
touch that namespace's activity tracker → wake that namespace if asleep →
proxy. Unchanged except the lookup now yields a namespace-scoped unit.

### The per-namespace unit ("Env") and registry

New package `internal/registry`:

```go
type Env struct {
    Namespace   string
    Scaler      *scaler.Scaler
    Power       *power.Manager
    Tracker     *activity.Tracker
    IdleTimeout time.Duration      // global default or per-ns override
}

type Registry struct {
    // mu-guarded: envs by namespace + host -> (Env, Upstream) table
}
```

- `Resolve(host) (*Env, routing.Upstream, bool)` — request hot path (RLock).
- `Rebuild(desired)` — full reconcile from a snapshot: **existing Envs are
  reused** (power state and idle timers must survive an unrelated namespace
  event), missing ones created, removed ones dropped. Called once at startup
  in static mode, on every namespace event in discovery mode.
- `ForEach(fn)` — for the idle loop.

`Scaler`, `power.Manager`, and `activity.Tracker` need no internal changes;
they are already per-namespace objects constructed once in `main.go`.

## External contract (previewkit-facing)

```yaml
# On each preview Namespace object:
metadata:
  labels:
    gatekeeper.dev/managed: "true"          # must match NAMESPACE_SELECTOR
  annotations:
    gatekeeper.dev/routes: |                # same shape as ROUTES_JSON values
      {"pr12.previews.example.com": {"service": "web", "port": 3000}}
    gatekeeper.dev/idle-timeout: "45m"      # optional per-namespace override
```

Rules:

- A route annotation **cannot reference another namespace** — the namespace is
  always the annotated object's. (Otherwise labeling your namespace would let
  you route traffic into someone else's.) Any `namespace` field in the
  annotation JSON is rejected.
- A labeled namespace with a missing/malformed routes annotation is **skipped
  with a warning** (and optionally a Kubernetes Event) — it is not managed at
  all. One bad namespace must never affect the others.
- Host collisions across namespaces: **oldest namespace (creationTimestamp)
  wins**, deterministic across restarts; the loser is logged loudly. (In
  static mode collisions are impossible — JSON object keys are unique.)

### New/changed environment variables

| Env | Default | Purpose |
|-----|---------|---------|
| `NAMESPACE_SELECTOR` | *(empty = static mode)* | Label selector for managed namespaces. Setting it enables discovery mode; mutually exclusive with `ROUTES_JSON`. |
| `ROUTES_ANNOTATION` | `gatekeeper.dev/routes` | Annotation holding a namespace's routes JSON. |
| `IDLE_TIMEOUT_ANNOTATION` | `gatekeeper.dev/idle-timeout` | Optional per-namespace idle override (discovery mode only). |
| `LEADER_ELECTION` | `false` | Enable active-passive mode. Off = current single-replica behavior, no new RBAC needed. |
| `POD_NAME`, `POD_NAMESPACE` | *(downward API)* | Election identity and Lease/self-exclusion namespace. `POD_NAMESPACE` falls back to `NAMESPACE` for legacy deployments. |
| `LEASE_NAME` | `gatekeeper` | Lease object name in `POD_NAMESPACE`. |
| `READY_PATH` | `/readyz` | Readiness probe path (see below). `HEALTH_PATH` stays as liveness. |

`ROUTES_JSON` entries gain an optional `"namespace"` field (static multi-ns,
used by e2e); entries without it default to `NAMESPACE`, so **existing
deployments parse identically**. `NAMESPACE` becomes required only when some
static route omits a namespace.

### Probes

- **Liveness** = `HEALTH_PATH` (`/healthz`), always 200. Unchanged.
- **Readiness** = `READY_PATH` (`/readyz`): 200 iff the discovery informer has
  synced (always 200 in static mode). **Not** leadership-coupled — all three
  replicas are Ready; only the leader-labeled pod receives Service traffic.
- Both paths bypass host routing and auth (same treatment `HEALTH_PATH` gets
  today); the README's "probe path must match" warning extends to `READY_PATH`.

### RBAC (new `deploy/cluster/` manifests; existing `deploy/` untouched)

| Scope | Resources | Verbs | Why |
|-------|-----------|-------|-----|
| ClusterRole | apps: deployments, statefulsets | get, list, watch, patch | sleep/wake across preview namespaces |
| ClusterRole | pods | list | wedged-pod fail-fast on wake |
| ClusterRole | namespaces | get, list, watch | discovery |
| ClusterRole (optional) | events | create | surface bad-annotation errors on the namespace |
| Role in `system` | coordination.k8s.io: leases | get, create, update | leader election |
| Role in `system` | pods | get, patch | leader labels/unlabels its own pod |

The namespace-annotation design keeps cluster-wide reads to Namespaces only
(no cluster-wide ConfigMap/Service read, which would be a much broader grant).

## Leadership lifecycle

- **Startup (every replica)**: best-effort remove own `role=leader` label
  (clears staleness after a crash-restart), start informer (discovery mode),
  join election. Standbys keep a warm informer cache but do **not** seed power
  state or run the idle loop.
- **OnStartedLeading**, strictly ordered: (1) seed every Env's power state and
  reset its idle timer (a standby's timers aged without traffic and must not
  sleep namespaces the previous leader was serving); (2) open the serving gate
  (`IsLeader`); (3) strip stale leader labels and label own pod. Proxied
  requests on any replica whose gate is closed - standbys, or a restarted pod
  still wearing a stale leader label - fail closed with 503 + Retry-After;
  probes and the auth callback stay served everywhere.
- **Namespace added while leading** (discovery): create Env + seed it.
- **OnStoppedLeading**: initiate the existing graceful shutdown and exit; the
  pod restarts as a standby. Guarantees at most one *intentionally* active
  proxy and reuses the restart-recovery path instead of a "demote" code path.
- **Idle loop** ticks only while leading — otherwise an idle standby (which
  receives no traffic by construction) would sleep every namespace.

Failover timeline (leader pod dies): endpoints drop the dead pod (seconds) →
a standby acquires the Lease (≤ ~15s) → labels itself → endpoints update →
traffic resumes; the new leader seeds asleep/awake state from the cluster.
Expected gap ≈ 5–20s of 503s. Websockets through the old leader drop. Idle
timers reset, so namespaces stay awake up to one extra `IDLE_TIMEOUT` — a
deliberate, conservative trade.

Known bounded imperfection: a partitioned-but-alive old leader can serve
alongside the new one for up to ~`renewDeadline` (10s) until its renew fails
and it exits. Consequences (double wake, premature sleep) are self-healing;
perfect fencing is not attainable with Lease election and not worth chasing.

## Phases (one PR each)

### PR 1 — per-namespace registry (static multi-ns core)

The structural refactor, no behavior change for existing deployments.

- `internal/routing`: `Upstream` gains `Namespace`; `Table` drops its single
  namespace field; URL building uses the upstream's namespace.
- `internal/registry` (new): `Env`, `Registry`, `Resolve`, `Rebuild`,
  `ForEach` as above.
- `internal/config`: per-route `namespace` + defaulting; `POD_NAMESPACE`
  (fallback `NAMESPACE`); validation updates.
- `internal/scaler`: self-exclusion becomes "skip `SELF_NAME` only when
  managing `POD_NAMESPACE`" — today a workload merely *named* `gatekeeper` in
  any managed namespace would be silently unmanaged. Logger gains a
  `namespace` field (it was implicit when there was one namespace).
- `internal/proxy`: `Handler` swaps its single power/readiness/tracker fields
  for a `Resolve` that returns the Env; steps 5–6 operate on the resolved Env.
- `internal/idle`: loop iterates `Registry.ForEach`, per-Env timeout
  (`<= 0` = that Env never auto-sleeps).
- `cmd/gatekeeper/main.go`: build registry from static config; seed each Env.
- Also: raise client-go rate limits (QPS 50 / burst 100). Readiness polling
  during wakes is ~2 LISTs per 500ms *per waking namespace*; the default
  QPS 5 would throttle a handful of concurrent wakes, and a fresh leader
  seeding N namespaces would stall on it too.

Acceptance: existing env vars produce byte-identical behavior; new e2e case
proves two static namespaces sleep/wake independently.

### PR 2 — HA (leader election + label steering)

- `LEADER_ELECTION`, `POD_NAME`, `LEASE_NAME`, `READY_PATH` config.
- Election wiring in `main.go` (client-go `leaderelection`,
  `ReleaseOnCancel: true`); callbacks per the lifecycle section.
- Pod label patch/unpatch helper (own pod only; `pods get,patch` Role).
- Handler: `READY_PATH` endpoint; idle loop gated on a `leading func() bool`.
- `deploy/cluster/`: Deployment (replicas 3, downward-API env, both probes,
  topology spread across zones, memory limit raised to 128Mi), leader-selector
  Service, Lease + pods Role/RoleBinding. Explicitly no PDB (see Decisions).
- e2e: kill the leader pod; assert traffic recovers within ~30s and slept
  state is re-derived correctly.

Acceptance: rolling restart of the 3-replica deployment completes cleanly
(this is exactly what the readiness-gating design would have broken) with a
single bounded traffic gap; `LEADER_ELECTION=false` continues to run exactly
as today with no Lease RBAC.

### PR 3 — namespace discovery

- `internal/discovery` (new): shared informer on Namespaces filtered by
  `NAMESPACE_SELECTOR`; on any event, rebuild the full desired state from the
  lister cache and hand it to `Registry.Rebuild` (full rebuild is microseconds
  at this scale and sidesteps incremental-update bugs); collision policy
  (oldest namespace wins); per-namespace parse-error isolation (+ optional
  Event); routes-annotation and idle-timeout-annotation parsing (reusing the
  `ROUTES_JSON` parser; reject in-annotation `namespace` fields).
- Config: discovery mode wiring, `ROUTES_JSON`/`NAMESPACE_SELECTOR` mutual
  exclusion.
- Readiness ties to informer cache sync; seed-on-add while leading.
- Debug endpoint `/_gatekeeper/routes` (behind the auth gate, unlike
  `HEALTH_PATH`): live host table + per-namespace power state, since there is
  no longer a single ConfigMap to eyeball.
- `deploy/cluster/`: ClusterRole/Binding for namespaces (+ optional events).
- e2e: label a namespace → routable in <5s; edit annotation → routes update;
  malformed annotation on ns A → ns B unaffected; delete ns → routes gone.

### PR 4 — packaging, docs, migration

- README: new "cluster mode" section, contract, RBAC, failover behavior;
  keep the single-namespace docs as the simple path.
- Migration guide for previewkit (`docs/migration-previewkit.md`):
  1. Deploy `deploy/cluster/` — inert until namespaces are labeled.
  2. Template vNext: add label + routes annotation to the Namespace; point the
     preview's ingress hosts at the central Service; add a NetworkPolicy
     allowing ingress from `system` (previews are default-deny);
     **stop stamping** the per-namespace gatekeeper resources.
  3. Cut over per preview (routing is per-host, so this is incremental):
     repoint ingress **and delete the old in-namespace gatekeeper in the same
     step** — a leftover one receives no traffic, goes idle, and would sleep a
     namespace the central gatekeeper believes is awake.
  4. Rollback per preview is the reverse: repoint ingress, re-stamp the old
     gatekeeper, unlabel the namespace.
- The central gatekeeper's API-server-egress NetworkPolicy moves to
  `system` (one copy instead of N).

## Test plan

- **Unit** (fake clientset, as today): registry rebuild preserves Env identity
  across unrelated events; collision determinism; annotation parsing incl.
  rejection cases; scaler self-exclusion scoping; idle loop with mixed per-Env
  timeouts and leadership gating; readiness states.
- **e2e** (extends `e2e/run.sh`): existing single-ns suite unchanged (legacy
  mode regression); new cluster-mode suite covering discovery, isolation
  (ns A sleeps while ns B serves), leader failover, and rolling restart.
- Leader-election timing is deliberately left to e2e rather than unit-mocked.

## Risks and mitigations

| Risk | Mitigation |
|------|------------|
| Dual-active proxy during a partition (≤ ~10s) | Bounded by renew deadline; consequences self-heal; documented. |
| Stale leader label after container crash | Startup label-clear; readiness fails while the process is down, pulling the pod from endpoints. |
| Central blast radius (one deployment fronts all previews) | The point of the HA work; plus per-namespace error isolation in discovery. |
| Leader restart bursts API calls (seeding N namespaces) | QPS/burst raise in PR 1. |
| Previewkit skew during migration | Legacy mode fully supported; per-preview cutover with per-preview rollback. |
| Missed netpol ingress-allow in a preview ns | Symptom is proxy timeouts to that ns only; add to migration checklist + README troubleshooting. |

## Out of scope (deliberate)

- Active-active replicas (informer-backed power state, Lease heartbeats for
  activity, per-namespace transition locks) — revisit only if one proxy pod is
  a measured bottleneck.
- Per-namespace auth tokens — global `AUTH_TOKEN` in v1; the annotation
  contract leaves room for it later.
- Ingress/Gateway-API integration for route discovery.

## Resolved questions (2026-07-03)

1. Events on bad annotations: **yes** — ClusterRole includes `events create`.
2. Central namespace: **`system`** (hardcoded in `deploy/cluster/`).
3. `NAMESPACE_SELECTOR`: **`gatekeeper.dev/managed=true`** as planned.
