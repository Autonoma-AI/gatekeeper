#!/usr/bin/env bash
# End-to-end test for Gatekeeper against a LOCAL Kubernetes cluster.
# Builds the image, deploys a sample app + Gatekeeper, then asserts the full
# auth -> sleep -> wake cycle through a port-forward.
#
#   ./e2e/run.sh                 # uses context "orbstack"
#   KUBE_CONTEXT=kind-foo ./e2e/run.sh
#
# Safety: refuses to run against anything but a known local context.
set -euo pipefail

CONTEXT="${KUBE_CONTEXT:-orbstack}"
NS="gatekeeper-e2e"
TOKEN="e2e-secret-token"
HOSTH="whoami.example.test"
LOGIN="http://login.example.test"
LOCAL_PORT="${LOCAL_PORT:-18080}"
IMAGE="gatekeeper:dev"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
base="http://localhost:${LOCAL_PORT}"

k() { kubectl --context "$CONTEXT" "$@"; }

case "$CONTEXT" in
  orbstack | docker-desktop | minikube | kind-*) ;;
  *) echo "Refusing to run E2E against non-local context '$CONTEXT'." >&2; exit 1 ;;
esac

FAILED=0
pass() { echo "  ✓ $1"; }
fail() { echo "  ✗ $1"; FAILED=1; }

echo "==> Building $IMAGE"
docker build -q -t "$IMAGE" "$ROOT" >/dev/null

echo "==> Applying manifests (context=$CONTEXT ns=$NS)"
k apply -f "$ROOT/e2e/e2e.yaml" >/dev/null

echo "==> Waiting for rollouts"
k -n "$NS" rollout status deploy/gatekeeper --timeout=120s
k -n "$NS" rollout status deploy/whoami --timeout=120s

echo "==> Port-forwarding svc/gatekeeper -> localhost:$LOCAL_PORT"
k -n "$NS" port-forward svc/gatekeeper "${LOCAL_PORT}:80" >/tmp/gk-pf.log 2>&1 &
PF_PID=$!
trap 'kill $PF_PID 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do
  curl -fsS "$base/healthz" >/dev/null 2>&1 && break
  sleep 1
done

echo "==> 1. health endpoint is unauthenticated"
if [ "$(curl -s -o /tmp/gk.out -w '%{http_code}' "$base/healthz")" = "200" ] && grep -q ok /tmp/gk.out; then
  pass "GET /healthz -> 200 ok"
else
  fail "health endpoint"
fi

echo "==> 2. unauthenticated request redirects to the login URL"
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Host: $HOSTH" "$base/dashboard")
loc=$(curl -s -o /dev/null -D - -H "Host: $HOSTH" "$base/dashboard" | tr -d '\r' | awk 'tolower($1)=="location:"{print $2}')
if [ "$code" = "302" ] && echo "$loc" | grep -q "^${LOGIN}?redirect="; then
  pass "302 -> $loc"
else
  fail "expected 302 to $LOGIN (got $code, location=$loc)"
fi

echo "==> 3. auth callback serves the cookie-setter page"
body=$(curl -s -H "Host: $HOSTH" "$base/_gatekeeper/auth?token=$TOKEN&next=/")
if echo "$body" | grep -q "gatekeeper_session" && echo "$body" | grep -q "domain=.example.test"; then
  pass "callback page sets gatekeeper_session on .example.test"
else
  fail "auth callback page"
fi

echo "==> 4. authenticated request is proxied to the app"
body=$(curl -s -H "Host: $HOSTH" -H "x-gatekeeper-token: $TOKEN" "$base/")
if echo "$body" | grep -qi "Hostname:" && echo "$body" | grep -q "X-Forwarded-Proto: http"; then
  pass "proxied to whoami; Host preserved + X-Forwarded-Proto set"
else
  echo "$body"
  fail "authenticated proxy"
fi

echo "==> 5. scales the app to zero after the idle timeout (~20s)"
sleep 32
rep=$(k -n "$NS" get deploy whoami -o jsonpath='{.spec.replicas}')
ann=$(k -n "$NS" get deploy whoami -o jsonpath='{.metadata.annotations.gatekeeper\.dev/wake-replicas}')
if [ "$rep" = "0" ] && [ "$ann" = "1" ]; then
  pass "whoami scaled to 0 (saved wake-replicas=$ann)"
else
  fail "expected whoami replicas=0 wake-replicas=1 (got replicas=$rep ann=$ann)"
fi
if [ "$(k -n "$NS" get deploy gatekeeper -o jsonpath='{.spec.replicas}')" = "1" ]; then
  pass "gatekeeper did NOT scale itself down"
else
  fail "gatekeeper scaled itself down"
fi

echo "==> 6. next request wakes the app and holds until ready"
code=$(curl -s -m 60 -o /tmp/gk.out -w '%{http_code}' -H "Host: $HOSTH" -H "x-gatekeeper-token: $TOKEN" "$base/")
rep=$(k -n "$NS" get deploy whoami -o jsonpath='{.spec.replicas}')
if [ "$code" = "200" ] && [ "$rep" = "1" ] && grep -qi Hostname /tmp/gk.out; then
  pass "request woke whoami (replicas=$rep) and returned 200"
else
  fail "wake-on-request (http=$code replicas=$rep)"
fi

echo
echo "==> Recent gatekeeper logs:"
k -n "$NS" logs deploy/gatekeeper --tail=20 2>/dev/null | sed 's/^/    /' || true

echo
if [ "$FAILED" = "0" ]; then
  echo "ALL E2E CHECKS PASSED ✅"
else
  echo "SOME E2E CHECKS FAILED ❌"
fi
echo "Namespace '$NS' left running for inspection."
echo "Tear down with: kubectl --context $CONTEXT delete ns $NS"
exit $FAILED
