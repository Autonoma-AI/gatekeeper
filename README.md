# Gatekeeper

Gatekeeper is a tiny per-namespace reverse proxy for Kubernetes that **scales your
workloads to zero when they are idle and wakes them on the next request** - holding
that request until the backend is ready, so the caller sees a slightly slow first
response instead of an error. It can optionally **authenticate** every request with
a shared token.

It ships as a single ~25 MB static binary on distroless, uses tens of MB of RAM,
starts instantly, and talks to the Kubernetes API with its own in-cluster
ServiceAccount. Everything is configured through environment variables.

## Why

Idle environments (per-branch/preview/staging/demo deployments, internal tools,
rarely-used services) burn CPU and memory around the clock. Gatekeeper sits in
front of them and:

- **Scales to zero** every selected Deployment and StatefulSet after an idle
  period, remembering each one's replica count.
- **Wakes on demand**: the next request restores those replicas, waits for the
  target Service's endpoints to become ready, then proxies through (Knative
  activator style). Websocket upgrades and streaming responses are supported.
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
| `NAMESPACE` | *(required)* | Namespace Gatekeeper manages. Inject via the downward API. |
| `ROUTES_JSON` | *(required)* | `{"host":{"service":"svc","port":80}, ...}` host -> upstream map. |
| `PORT` | `8080` | Listen port. |
| `HEALTH_PATH` | `/healthz` | Unauthenticated health/probe path. |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error`. JSON logs to stdout. |

### Scale-to-zero

| Env | Default | Purpose |
|-----|---------|---------|
| `TARGET_SELECTOR` | `gatekeeper.dev/scale-to-zero=true` | Label selector for managed Deployments/StatefulSets. Empty selects every workload in the namespace. |
| `SELF_NAME` | `gatekeeper` | Workload name Gatekeeper never scales (itself). |
| `WAKE_REPLICAS_ANNOTATION` | `gatekeeper.dev/wake-replicas` | Annotation storing the pre-sleep replica count. |
| `IDLE_TIMEOUT` | `30m` | Idle duration before scaling to zero (Go duration). |
| `IDLE_CHECK_INTERVAL` | `30s` | How often idleness is checked. |
| `WAKE_TIMEOUT` | `90s` | Max time to hold a request while waking; then 503 + `Retry-After`. |

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

Gatekeeper runs as its own ServiceAccount and needs a namespaced Role:

```yaml
rules:
  - apiGroups: ["apps"]
    resources: ["deployments", "statefulsets"]
    verbs: ["get", "list", "watch", "patch"]
  - apiGroups: ["discovery.k8s.io"]
    resources: ["endpointslices"]
    verbs: ["get", "list"]
```

`patch` on the workloads sets `spec.replicas` and the wake annotation in one merge
patch; `endpointslices` are polled for readiness. `deploy/` contains the full set
(ServiceAccount, Role, RoleBinding).

> If the namespace runs a default-deny egress NetworkPolicy, Gatekeeper also needs
> egress to the Kubernetes API server (see `deploy/networkpolicy-apiserver-egress.yaml`),
> or every scale call will hang.

## Develop

```sh
make all      # gofmt check + vet + test + build
make docker   # build the container image
./e2e/run.sh  # end-to-end test on a local cluster (OrbStack / kind / docker-desktop)
```

Go 1.26+. The module is `github.com/autonoma-ai/gatekeeper`.

## License

MIT - see [LICENSE](LICENSE).
