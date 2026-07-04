# Migrating previewkit from per-namespace Gatekeepers to cluster mode

Today previewkit stamps a full Gatekeeper (Deployment, ServiceAccount, Role,
RoleBinding, routes ConfigMap, Service, NetworkPolicy) into every `preview-*`
namespace. Cluster mode replaces all of that with **one Gatekeeper install**
(namespace `system`, 3 replicas, leader election) that **discovers** preview
namespaces by label and routes by annotation.

Because routing is per-host at the ingress, the migration is **incremental and
per-preview**: old and new Gatekeepers coexist, each preview cuts over (and can
roll back) independently, and nothing needs a flag day.

## 0. Prerequisites

- Gatekeeper image with cluster mode (PRs #1-#3; any tag from this repo's main).
- Cluster-admin on the previewkit EKS cluster (ClusterRole/Binding creation).
- Nothing else changes: workload labels (`gatekeeper.dev/scale-to-zero=true`),
  wake annotations, and dependency ordering behave exactly as before - the
  same code runs, scoped per namespace.

## 1. Deploy the central Gatekeeper (inert)

```sh
kubectl apply -f deploy/cluster/
```

This creates namespace `system`, the 3-replica Deployment, the leader-selecting
Service, RBAC, and the API-server-egress NetworkPolicy. It is **inert**: with no
namespace labeled `gatekeeper.dev/managed=true`, it routes nothing.

Verify before proceeding:

```sh
kubectl -n system rollout status deploy/gatekeeper          # 3/3 available
kubectl -n system get pods -l gatekeeper.dev/role=leader    # exactly one
```

## 2. previewkit template vNext

For each preview namespace, the template changes are:

**Add** to the Namespace object:

```yaml
metadata:
  labels:
    gatekeeper.dev/managed: "true"
  annotations:
    gatekeeper.dev/routes: |
      { "<preview-host>": { "service": "<svc>", "port": <port> }, ... }
    # optional: gatekeeper.dev/idle-timeout: "45m"
```

**Add** (only if preview namespaces run default-deny ingress) an
ingress-allow from the central Gatekeeper - see the commented example in
`deploy/cluster/networkpolicy.yaml`. The proxy now connects from the `system`
namespace, not from inside the preview. *Symptom when missing: that preview's
requests time out at the proxy while everything else looks healthy.*

**Change** the preview's Ingress/Gateway backends for its hosts to
`gatekeeper.system.svc` (port 80).

**Stop stamping** the per-namespace Gatekeeper resources: Deployment,
ServiceAccount, Role, RoleBinding, `gatekeeper-routes` ConfigMap, Service, and
the per-namespace `allow-gatekeeper-apiserver-egress` NetworkPolicy.

New previews created from vNext are handled by the central Gatekeeper from
birth; nothing further to do for them.

## 3. Cutting over existing previews

Per preview, apply the same three changes - label+annotate the namespace,
repoint the ingress, delete the old in-namespace Gatekeeper - with one hard
rule:

> **Repoint the ingress and delete the old Gatekeeper in the same step.**
> A leftover in-namespace Gatekeeper receives no traffic once the ingress
> moves, so after its idle timeout it will scale the namespace to zero while
> the central Gatekeeper - which is serving that preview's traffic - still
> believes it is awake. Requests then hang against a dead backend until the
> central one's state self-corrects. Deleting the old Gatekeeper (or at
> minimum setting its `IDLE_TIMEOUT=0` first) closes that window.

Suggested order per preview (script-friendly, ~seconds of proxy blip):

```sh
NS=preview-acme-pr-12

# 1. Hand the namespace to the central gatekeeper.
kubectl annotate ns "$NS" gatekeeper.dev/routes="$(routes_json_for "$NS")"
kubectl label ns "$NS" gatekeeper.dev/managed=true
# (+ apply the ingress-allow NetworkPolicy if previews are default-deny)

# 2. Atomically: repoint ingress + remove the old gatekeeper.
kubectl -n "$NS" patch ingress preview --type=json -p "$(ingress_backend_patch)"
kubectl -n "$NS" delete deploy/gatekeeper svc/gatekeeper cm/gatekeeper-routes \
  sa/gatekeeper role/gatekeeper rolebinding/gatekeeper --ignore-not-found

# 3. Verify.
curl -fsS -H "Host: <preview-host>" http://<ingress> >/dev/null
```

Both Gatekeepers managing the same namespace for the few seconds between step
1 and step 2 is harmless: wake operations are idempotent, and neither sleeps
an active namespace that fast.

## 4. Rollback (per preview)

The reverse, same atomicity rule:

```sh
kubectl label ns "$NS" gatekeeper.dev/managed-        # central lets go instantly
# re-stamp the old per-namespace gatekeeper resources (previous template)
# repoint the ingress back to the in-namespace gatekeeper Service
```

## 5. Cleanup

Once every preview is on vNext and the old stamps are drained, previewkit can
drop the per-namespace Gatekeeper resources from its template entirely. The
single-namespace mode remains supported by this repo (`deploy/`), so there is
no forced timeline.

## Verification & debugging

```sh
# Who is leader, and is the cache synced?
kubectl -n system get pods -l gatekeeper.dev/role=leader
kubectl -n system get pods -o wide            # all 3 Ready (readiness = cache sync)

# What does gatekeeper think it routes? The /_gatekeeper/routes endpoint only
# exists when AUTH_TOKEN is set (otherwise it would enumerate every preview's
# secret hostname). Previews run auth-off, so inspect the source of truth -
# the namespace annotations - directly:
kubectl get ns -l gatekeeper.dev/managed=true \
  -o custom-columns='NS:.metadata.name,ROUTES:.metadata.annotations.gatekeeper\.dev/routes'

# Why is a namespace not managed? Bad annotations surface as Events:
kubectl get events -n default --field-selector reason=InvalidRoutes
kubectl get events -n default --field-selector reason=HostCollision
```

Failure modes worth knowing while operating:

- **Preview times out through the proxy, others fine** → missing ingress-allow
  NetworkPolicy in that preview namespace (step 2).
- **All previews 503 with `Retry-After`** → no seeded leader (election churn or
  API-server trouble); check the Lease: `kubectl -n system get lease gatekeeper`.
- **Leader failover** drops websockets and resets idle timers (namespaces stay
  awake up to one extra idle timeout) - by design, bounded, self-healing.
- **A preview never sleeps** → check for a per-namespace
  `gatekeeper.dev/idle-timeout: "0"` override on the Namespace annotations.
