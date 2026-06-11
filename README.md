# Gatekeeper

A tiny per-namespace reverse proxy for Autonoma preview environments. One Gatekeeper
pod runs in each preview namespace and replaces the old stock-nginx proxy
(`previewkit-nginx`). It does three jobs:

1. **Authenticates every request.** Traffic must carry the environment's bypass token,
   either the `x-previewkit-bypass` header (service-to-service) or the `pk_session`
   cookie (browsers). Unauthenticated browsers are redirected to the main app's
   `/preview-auth` page, which checks org membership, issues the token, and bounces
   back to set the cookie. Gatekeeper itself serves that cookie-setting `/preview-auth`
   page on the preview host.
2. **Scales the namespace to zero when idle.** It tracks the last request time; after
   `IDLE_TIMEOUT` (default 30m) it scales every previewkit-managed Deployment and
   StatefulSet (apps, Redis, Postgres, Mongo) to zero, saving each one's replica count
   on a `previewkit.dev/wake-replicas` annotation. It never scales itself.
3. **Wakes on demand and holds the request.** The next request restores every
   workload's replica count, then holds the connection (Knative-activator style),
   waiting for the target app's Service endpoints to become ready before proxying the
   real response. If the wake exceeds `WAKE_TIMEOUT` (default 90s) it returns
   `503` with `Retry-After`.

It is built in Go: a static binary on distroless (~15-25MB), tens of MB of RAM, instant
start. `net/http/httputil.ReverseProxy` handles streaming and websocket upgrades;
`k8s.io/client-go` does the scaling via the pod's in-cluster ServiceAccount.

## How it fits in

Gatekeeper sits behind the shared ALB Gateway + ingress-nginx, one instance per preview
namespace. The Autonoma `previewkit` deployer creates Gatekeeper's Deployment, Service,
ServiceAccount, Role, RoleBinding, ConfigMap, and the API-server egress NetworkPolicy for
each environment, and points the per-app Ingress at Gatekeeper's Service. The token
issuing/login flow lives in the Autonoma app (`previewAccess.issueToken` + the
`/preview-auth` UI route); Gatekeeper only validates the token and sets the cookie.

## Configuration (environment variables)

| Var | Default | Purpose |
| --- | --- | --- |
| `PORT` | `8080` | Listen port (nonroot cannot bind <1024). |
| `NAMESPACE` | (required) | The preview namespace. Injected via the downward API. |
| `BYPASS_TOKEN` | (required) | Plaintext per-environment token compared against the cookie/header. |
| `COOKIE_DOMAIN` | (required) | Preview domain; the `pk_session` cookie is set on `.<domain>`. |
| `APP_URL` | (required) | Main app origin used to build the login redirect. |
| `ROUTES_JSON` | (required) | `{"host":{"service":"app","port":3000}, ...}` host -> upstream map. |
| `IDLE_TIMEOUT` | `30m` | Idle duration before scaling to zero (Go duration). |
| `IDLE_CHECK_INTERVAL` | `30s` | How often the idle loop checks. |
| `WAKE_TIMEOUT` | `90s` | Max time to hold a request while waking. |
| `SELF_APP_LABEL` | `gatekeeper` | `app` label value Gatekeeper excludes from scaling (itself). |
| `MANAGED_LABEL_SELECTOR` | `previewkit.dev/managed-by=previewkit` | Selector for managed workloads. |
| `HEALTH_PATH` | `/gatekeeper-health` | Unauthenticated readiness/liveness path. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |

## RBAC

Gatekeeper needs a namespaced Role (see `deploy/role.yaml`):

```
apps: deployments, statefulsets    -> get, list, watch, patch
"":   endpoints, pods              -> get, list
```

`patch` on the workloads sets both `spec.replicas` and the wake annotation in one merge
patch; `endpoints` is polled for readiness.

> **Network policy gotcha:** preview namespaces typically run a default-deny egress
> policy that excludes RFC1918 ranges, which blocks the in-cluster API server. Gatekeeper
> needs the `allow-gatekeeper-apiserver-egress` policy (`deploy/networkpolicy-apiserver-egress.yaml`)
> or every scale call hangs.

## Develop

```sh
make all        # gofmt check + vet + test + build
make test
make docker     # build the image
```

Standalone deploy example manifests live in `deploy/` (namespace `preview-example`).
