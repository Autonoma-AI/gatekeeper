# Migrating previewkit from per-namespace Gatekeepers to cluster mode

Today previewkit stamps a full Gatekeeper (Deployment, ServiceAccount, Role,
RoleBinding, routes ConfigMap, Service, NetworkPolicy) into every `preview-*`
namespace. Cluster mode replaces all of that with **one Gatekeeper install**
(namespace `system`, 3 replicas, leader election) that **discovers** preview
namespaces by label and routes by annotation.

The central Gatekeeper sits behind **one wildcard Ingress**,
`*.preview.autonoma.app` → `gatekeeper.system.svc`. Existing previews keep
their own **exact-host** Ingresses (`<pr>.preview.autonoma.app` → their
in-namespace Gatekeeper) until they are cut over. nginx always prefers an
exact host over the wildcard, so each preview flips the instant its exact-host
Ingress is deleted and nginx falls through to the wildcard - nothing is
"repointed". The migration is therefore **incremental**: old and new
Gatekeepers coexist, each preview cuts over (and can roll back) independently,
and nothing needs a flag day.

## 0. Prerequisites

- Gatekeeper image with cluster mode - any tag built from this repo's main
  (leader election + label/annotation discovery).
- Cluster-admin on the previewkit EKS cluster (ClusterRole/Binding creation).
- Nothing else changes: workload labels (`gatekeeper.dev/scale-to-zero=true`),
  wake annotations, and dependency ordering behave exactly as before - the
  same code runs, scoped per namespace.
- The previewkit-specific pieces - the wildcard Ingress and the
  `migrate-existing-previews.sh` cutover script - live in the **agent repo**
  under `deployment/previewkit/cluster/gatekeeper/`, because `deployment/` is
  autonoma-internal. This repo ships only the generic cluster-mode manifests
  (`deploy/cluster/`) and the Gatekeeper code.

## 1. Apply the central install manifests (inert)

From this repo:

```sh
kubectl apply -f deploy/cluster/
```

This creates namespace `system`, the 3-replica Deployment, the leader-selecting
Service, RBAC, and the API-server-egress NetworkPolicy. It is **inert**: with no
namespace labeled `gatekeeper.dev/managed=true`, it routes nothing, and every
existing preview's exact-host Ingress still wins over the (not-yet-applied)
wildcard.

Verify before proceeding:

```sh
kubectl -n system rollout status deploy/gatekeeper          # 3/3 available
kubectl -n system get pods -l gatekeeper.dev/role=leader    # exactly one
```

## 2. Ship the previewkit cluster-mode change

This is the deploy-path change in the agent repo. Two parts:

**The wildcard Ingress** (one, in namespace `system`) routes any preview host
that has no exact-host Ingress of its own to the central Gatekeeper:

```yaml
# agent repo: deployment/previewkit/cluster/gatekeeper/ingress.yaml
- host: "*.preview.autonoma.app"
  http:
    paths:
      - { path: /, pathType: Prefix,
          backend: { service: { name: gatekeeper, port: { number: 80 } } } }
```

Applying it is safe at any point: existing previews still resolve to their own
exact-host Ingresses (exact beats wildcard), so only brand-new vNext previews
- which have no exact-host Ingress - fall through to central.

**previewkit template vNext.** For each new preview namespace, the template:

- **Adds** to the Namespace object:

  ```yaml
  metadata:
    labels:
      gatekeeper.dev/managed: "true"
    annotations:
      gatekeeper.dev/routes: |
        { "<preview-host>": { "service": "<svc>", "port": <port> }, ... }
      # optional: gatekeeper.dev/idle-timeout: "45m"
  ```

- **Adds** (only if preview namespaces run default-deny ingress) an
  ingress-allow from the central Gatekeeper - see the commented example in
  `deploy/cluster/networkpolicy.yaml`. The proxy now connects from the `system`
  namespace, not from inside the preview. *Symptom when missing: that preview's
  requests time out at the proxy while everything else looks healthy.*

- **Stops creating** the per-app exact-host Ingress - the wildcard covers it.

- **Stops stamping** the per-namespace Gatekeeper resources: Deployment,
  ServiceAccount, Role, RoleBinding, `gatekeeper-routes` ConfigMap, Service, and
  the per-namespace `allow-gatekeeper-apiserver-egress` NetworkPolicy.

New previews created from vNext are handled by the central Gatekeeper from
birth; nothing further to do for them.

## 3. Migrate existing previews

Previews created before vNext still have their exact-host Ingress and their old
in-namespace Gatekeeper, so they are untouched by step 2. Cutting one over
means doing three things **atomically**, per namespace:

1. **Handoff** - label + annotate the namespace so the central Gatekeeper
   adopts it (and apply the ingress-allow NetworkPolicy if previews are
   default-deny).
2. **Cutover** - delete the preview's exact-host Ingress. nginx falls through
   to the wildcard, so the host now resolves to the central Gatekeeper. No
   backend is repointed and there is no unrouted window - the wildcard is
   simply the next-best match.
3. **Teardown** - delete the old in-namespace Gatekeeper.

Doing this by hand across a fleet is error-prone and, because of the race
below, unsafe if the steps drift apart - so it is a script:

```sh
# Agent repo; deployment/ is autonoma-internal. Dry-run by default:
deployment/previewkit/cluster/gatekeeper/migrate-existing-previews.sh
# Execute once the printed plan looks right:
deployment/previewkit/cluster/gatekeeper/migrate-existing-previews.sh --apply
```

Run `--apply` **promptly** after step 2 ships, so the two Gatekeepers don't run
split-brain over the fleet for long. Then **re-run it for stragglers** -
previews created mid-rollout, or any that errored the first time. It is
idempotent: already-migrated namespaces are skipped.

> **Handoff, cutover, and teardown are one atomic unit per namespace.**
> A leftover in-namespace Gatekeeper receives no traffic once its exact-host
> Ingress is gone, so after its idle timeout it will scale the namespace to
> zero while the central Gatekeeper - which is now serving that preview's
> traffic - still believes it is awake. Requests then hang against a dead
> backend until the central one's state self-corrects. The script tears the old
> Gatekeeper down in the same step it cuts traffic over (at minimum it sets the
> old `IDLE_TIMEOUT=0` first), closing that window. This dual-idle-loop race is
> the whole reason the cutover is a script and not a runbook.

Both Gatekeepers managing the same namespace for the few seconds between
handoff and teardown is harmless: wake operations are idempotent, and neither
sleeps an active namespace that fast.

## 4. Rollback (per preview)

The exact-host-beats-wildcard property makes this clean. Recreate the preview's
exact-host Ingress (nginx immediately prefers it over the wildcard, so traffic
returns to the namespace) and re-stamp the per-namespace Gatekeeper from the
previous template - then drop the label so central lets go:

```sh
# 1. restore the in-namespace path first: re-stamp the old gatekeeper + its
#    exact-host Ingress (previous template)
# 2. then hand it back:
kubectl label ns "$NS" gatekeeper.dev/managed-        # central lets go instantly
```

Same atomicity rule in reverse: restore the in-namespace Gatekeeper and its
exact-host Ingress **before** removing the label, or the host briefly falls
through to a central Gatekeeper that no longer manages it.

## 5. Cleanup

Once every preview is on vNext and the old stamps are drained, previewkit can
drop the per-namespace Gatekeeper resources from its template entirely, and the
migration script can be retired. The single-namespace mode remains supported by
this repo (`deploy/`), so there is no forced timeline.

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
  NetworkPolicy in that preview namespace.
- **A migrated preview still hits the old backend** → its exact-host Ingress was
  not deleted, so nginx still prefers it over the wildcard; re-run the script.
- **All previews 503 with `Retry-After`** → no seeded leader (election churn or
  API-server trouble); check the Lease: `kubectl -n system get lease gatekeeper`.
- **Leader failover** drops websockets and resets idle timers (namespaces stay
  awake up to one extra idle timeout) - by design, bounded, self-healing.
- **A preview never sleeps** → check for a per-namespace
  `gatekeeper.dev/idle-timeout: "0"` override on the Namespace annotations.
