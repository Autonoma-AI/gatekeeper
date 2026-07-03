#!/usr/bin/env bash
# End-to-end test for Gatekeeper against a LOCAL Kubernetes cluster.
# Builds the image, deploys a sample app in two namespaces + one Gatekeeper
# managing both, then asserts the full auth -> sleep -> wake cycle through a
# port-forward, including that the namespaces sleep and wake independently.
#
#   ./e2e/run.sh                 # uses context "orbstack"
#   KUBE_CONTEXT=kind-foo ./e2e/run.sh
#
# Safety: refuses to run against anything but a known local context.
set -euo pipefail

CONTEXT="${KUBE_CONTEXT:-orbstack}"
NS="gatekeeper-e2e"
NSB="gatekeeper-e2e-b"
TOKEN="e2e-secret-token"
HOSTH="whoami.example.test"
HOSTB="whoami-b.example.test"
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

echo "==> Applying manifests (context=$CONTEXT ns=$NS,$NSB)"
k apply -f "$ROOT/e2e/e2e.yaml" >/dev/null

echo "==> Waiting for rollouts"
k -n "$NS" rollout status deploy/gatekeeper --timeout=120s
k -n "$NS" rollout status deploy/whoami --timeout=120s
k -n "$NSB" rollout status deploy/whoami --timeout=120s

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

echo "==> 5. request for the second namespace's host is proxied cross-namespace"
body=$(curl -s -H "Host: $HOSTB" -H "x-gatekeeper-token: $TOKEN" "$base/")
if echo "$body" | grep -qi "Hostname:" && echo "$body" | grep -q "X-Forwarded-Proto: http"; then
  pass "proxied to whoami in $NSB"
else
  echo "$body"
  fail "cross-namespace proxy"
fi

echo "==> 6. scales both namespaces to zero after the idle timeout (~20s)"
sleep 32
rep=$(k -n "$NS" get deploy whoami -o jsonpath='{.spec.replicas}')
ann=$(k -n "$NS" get deploy whoami -o jsonpath='{.metadata.annotations.gatekeeper\.dev/wake-replicas}')
if [ "$rep" = "0" ] && [ "$ann" = "1" ]; then
  pass "$NS/whoami scaled to 0 (saved wake-replicas=$ann)"
else
  fail "expected $NS/whoami replicas=0 wake-replicas=1 (got replicas=$rep ann=$ann)"
fi
repb=$(k -n "$NSB" get deploy whoami -o jsonpath='{.spec.replicas}')
annb=$(k -n "$NSB" get deploy whoami -o jsonpath='{.metadata.annotations.gatekeeper\.dev/wake-replicas}')
if [ "$repb" = "0" ] && [ "$annb" = "1" ]; then
  pass "$NSB/whoami scaled to 0 (saved wake-replicas=$annb)"
else
  fail "expected $NSB/whoami replicas=0 wake-replicas=1 (got replicas=$repb ann=$annb)"
fi
if [ "$(k -n "$NS" get deploy gatekeeper -o jsonpath='{.spec.replicas}')" = "1" ]; then
  pass "gatekeeper did NOT scale itself down"
else
  fail "gatekeeper scaled itself down"
fi

echo "==> 7. next request wakes the first namespace only (isolation)"
code=$(curl -s -m 60 -o /tmp/gk.out -w '%{http_code}' -H "Host: $HOSTH" -H "x-gatekeeper-token: $TOKEN" "$base/")
rep=$(k -n "$NS" get deploy whoami -o jsonpath='{.spec.replicas}')
if [ "$code" = "200" ] && [ "$rep" = "1" ] && grep -qi Hostname /tmp/gk.out; then
  pass "request woke $NS/whoami (replicas=$rep) and returned 200"
else
  fail "wake-on-request (http=$code replicas=$rep)"
fi
repb=$(k -n "$NSB" get deploy whoami -o jsonpath='{.spec.replicas}')
if [ "$repb" = "0" ]; then
  pass "$NSB/whoami stayed asleep (replicas=$repb)"
else
  fail "waking $NS must not wake $NSB (got replicas=$repb)"
fi

echo "==> 8. the second namespace wakes independently on its own host"
code=$(curl -s -m 60 -o /tmp/gk.out -w '%{http_code}' -H "Host: $HOSTB" -H "x-gatekeeper-token: $TOKEN" "$base/")
repb=$(k -n "$NSB" get deploy whoami -o jsonpath='{.spec.replicas}')
if [ "$code" = "200" ] && [ "$repb" = "1" ] && grep -qi Hostname /tmp/gk.out; then
  pass "request woke $NSB/whoami (replicas=$repb) and returned 200"
else
  fail "wake-on-request for $NSB (http=$code replicas=$repb)"
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
echo "Namespaces '$NS' and '$NSB' left running for inspection."
echo "Tear down with: kubectl --context $CONTEXT delete ns $NS $NSB"
exit $FAILED
