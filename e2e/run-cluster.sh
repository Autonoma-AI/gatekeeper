#!/usr/bin/env bash
# End-to-end test for Gatekeeper CLUSTER MODE against a LOCAL cluster:
# 3 replicas with leader election, traffic steered by the leader pod label.
# Asserts exactly-one-leader, proxying + sleep across two app namespaces,
# then force-kills the leader and asserts failover: a standby takes over,
# re-derives the slept state from the cluster, and wakes on request.
#
#   ./e2e/run-cluster.sh             # uses context "orbstack"
#   KUBE_CONTEXT=kind-foo ./e2e/run-cluster.sh
#
# Safety: refuses to run against anything but a known local context.
set -euo pipefail

CONTEXT="${KUBE_CONTEXT:-orbstack}"
SYS="gatekeeper-e2e-sys"
NSA="gatekeeper-e2e-a"
NSB="gatekeeper-e2e-b"
HOSTA="whoami-a.example.test"
HOSTB="whoami-b.example.test"
LOCAL_PORT="${LOCAL_PORT:-18081}"
IMAGE="gatekeeper:dev"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
base="http://localhost:${LOCAL_PORT}"
LEADER_SELECTOR="gatekeeper.dev/role=leader"

k() { kubectl --context "$CONTEXT" "$@"; }

case "$CONTEXT" in
  orbstack | docker-desktop | minikube | kind-*) ;;
  *) echo "Refusing to run E2E against non-local context '$CONTEXT'." >&2; exit 1 ;;
esac

FAILED=0
pass() { echo "  ✓ $1"; }
fail() { echo "  ✗ $1"; FAILED=1; }

PF_PID=""
pf_start() {
  k -n "$SYS" port-forward svc/gatekeeper "${LOCAL_PORT}:80" >/tmp/gk-cluster-pf.log 2>&1 &
  PF_PID=$!
  for _ in $(seq 1 30); do
    curl -fsS "$base/healthz" >/dev/null 2>&1 && return 0
    sleep 1
  done
  echo "port-forward did not become ready" >&2
  return 1
}
pf_stop() { [ -n "$PF_PID" ] && { kill "$PF_PID" 2>/dev/null || true; wait "$PF_PID" 2>/dev/null || true; PF_PID=""; }; }
trap 'pf_stop' EXIT

leader_pods() { k -n "$SYS" get pods -l "$LEADER_SELECTOR" -o jsonpath='{.items[*].metadata.name}'; }

# wait_for_one_leader [excluded-name] -> echoes the leader pod name
wait_for_one_leader() {
  local exclude="${1:-}" names
  for _ in $(seq 1 45); do
    names=$(leader_pods)
    # -eq, not string =: BSD wc pads its output with spaces.
    if [ "$(echo "$names" | wc -w)" -eq 1 ] && [ "$names" != "$exclude" ]; then
      echo "$names"
      return 0
    fi
    sleep 1
  done
  return 1
}

echo "==> Building $IMAGE"
docker build -q -t "$IMAGE" "$ROOT" >/dev/null

echo "==> Applying manifests (context=$CONTEXT ns=$SYS,$NSA,$NSB)"
k apply -f "$ROOT/e2e/cluster.yaml" >/dev/null

echo "==> Waiting for rollouts"
# 3/3 available also proves readiness is NOT leadership-gated: a rollout could
# never complete if standbys were unready.
k -n "$SYS" rollout status deploy/gatekeeper --timeout=120s
k -n "$NSA" rollout status deploy/whoami --timeout=120s
k -n "$NSB" rollout status deploy/whoami --timeout=120s

echo "==> 1. exactly one pod is labeled leader"
if LEADER=$(wait_for_one_leader); then
  pass "leader elected and labeled: $LEADER"
else
  fail "expected exactly one leader-labeled pod (got: '$(leader_pods)')"
fi

echo "==> 2. traffic reaches both app namespaces through the leader"
pf_start
# -m 60: on a re-run against a leftover slept environment these wake first.
okA=$(curl -s -m 60 -o /dev/null -w '%{http_code}' -H "Host: $HOSTA" "$base/")
okB=$(curl -s -m 60 -o /dev/null -w '%{http_code}' -H "Host: $HOSTB" "$base/")
if [ "$okA" = "200" ] && [ "$okB" = "200" ]; then
  pass "proxied to $NSA and $NSB (200/200)"
else
  fail "proxying through leader (a=$okA b=$okB)"
fi

echo "==> 3. both namespaces scale to zero after the idle timeout (~20s)"
sleep 32
repA=$(k -n "$NSA" get deploy whoami -o jsonpath='{.spec.replicas}')
repB=$(k -n "$NSB" get deploy whoami -o jsonpath='{.spec.replicas}')
if [ "$repA" = "0" ] && [ "$repB" = "0" ]; then
  pass "both namespaces asleep"
else
  fail "expected both at 0 (a=$repA b=$repB)"
fi

echo "==> 4. force-kill the leader ($LEADER); a standby takes over"
pf_stop
k -n "$SYS" delete pod "$LEADER" --grace-period=0 --force --wait=false >/dev/null 2>&1
if NEW_LEADER=$(wait_for_one_leader "$LEADER"); then
  pass "new leader labeled: $NEW_LEADER"
else
  fail "no new leader within 45s (got: '$(leader_pods)')"
fi

echo "==> 5. new leader re-derived slept state and wakes on request"
pf_start
code=$(curl -s -m 60 -o /tmp/gk-cluster.out -w '%{http_code}' -H "Host: $HOSTA" "$base/")
repA=$(k -n "$NSA" get deploy whoami -o jsonpath='{.spec.replicas}')
if [ "$code" = "200" ] && [ "$repA" = "1" ] && grep -qi Hostname /tmp/gk-cluster.out; then
  pass "request woke $NSA/whoami through the new leader (200)"
else
  fail "wake-on-request after failover (http=$code replicas=$repA)"
fi
repB=$(k -n "$NSB" get deploy whoami -o jsonpath='{.spec.replicas}')
if [ "$repB" = "0" ]; then
  pass "$NSB stayed asleep through the failover (isolation preserved)"
else
  fail "failover must not wake $NSB (got replicas=$repB)"
fi

echo "==> 6. still exactly one leader-labeled pod"
if [ "$(leader_pods | wc -w)" -eq 1 ]; then
  pass "single leader label: $(leader_pods)"
else
  fail "expected exactly one leader label (got: '$(leader_pods)')"
fi

echo
echo "==> Recent leader logs:"
k -n "$SYS" logs "pod/$(leader_pods)" --tail=15 2>/dev/null | sed 's/^/    /' || true

echo
if [ "$FAILED" = "0" ]; then
  echo "ALL CLUSTER-MODE E2E CHECKS PASSED ✅"
else
  echo "SOME CLUSTER-MODE E2E CHECKS FAILED ❌"
fi
echo "Namespaces '$SYS', '$NSA', '$NSB' + ClusterRole/Binding 'gatekeeper-e2e' left for inspection."
echo "Tear down with: kubectl --context $CONTEXT delete ns $SYS $NSA $NSB; kubectl --context $CONTEXT delete clusterrolebinding gatekeeper-e2e; kubectl --context $CONTEXT delete clusterrole gatekeeper-e2e"
exit $FAILED
