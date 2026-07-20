#!/usr/bin/env bash
# Run the gateway locally with auth enforced, against a throwaway mock OIDC
# issuer. Dev only — never use the mock issuer for anything real.
#
# Boots scripts/mock-oidc in the background, waits for its discovery endpoint,
# then runs the gateway in the foreground wired to it. The minted bearer token
# is written to $TOKEN_FILE so you can call the API:
#
#   ./scripts/dev-auth.sh                 # in one terminal
#   curl -H "Authorization: Bearer $(cat /tmp/farcaster-dev-token)" \
#        localhost:8080/v1/agents          # in another
#
# Set FARCASTER_DATABASE_URL before running to back it with Postgres instead of
# the in-memory store.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ISSUER="${ISSUER:-http://127.0.0.1:9099}"
AUDIENCE="${AUDIENCE:-farcaster}"
TOKEN_FILE="${TOKEN_FILE:-/tmp/farcaster-dev-token}"

cleanup() { [ -n "${MOCK_PID:-}" ] && kill "$MOCK_PID" 2>/dev/null || true; }
trap cleanup EXIT INT TERM

echo "dev-auth: starting mock OIDC issuer at $ISSUER"
( cd "$ROOT/scripts/mock-oidc" && go run . \
    -issuer "$ISSUER" -audience "$AUDIENCE" -token-file "$TOKEN_FILE" ) &
MOCK_PID=$!

# Wait for discovery to answer before starting the gateway (it does OIDC
# discovery at boot and exits if the issuer is unreachable).
for _ in $(seq 1 30); do
  if curl -fsS -o /dev/null "$ISSUER/.well-known/openid-configuration" 2>/dev/null; then break; fi
  sleep 0.5
done
curl -fsS -o /dev/null "$ISSUER/.well-known/openid-configuration" 2>/dev/null || { echo "dev-auth: mock OIDC issuer did not come up at $ISSUER; aborting" >&2; exit 1; }

echo "dev-auth: token written to $TOKEN_FILE"
echo "dev-auth: call the API with:  curl -H \"Authorization: Bearer \$(cat $TOKEN_FILE)\" localhost:8080/v1/agents"
echo "dev-auth: starting gateway on :8080 (Ctrl-C to stop both)"

cd "$ROOT"
FARCASTER_OIDC_ISSUER="$ISSUER" \
FARCASTER_OIDC_AUDIENCE="$AUDIENCE" \
FARCASTER_OIDC_TENANT_CLAIM="${FARCASTER_OIDC_TENANT_CLAIM:-tenant}" \
FARCASTER_OIDC_USER_CLAIM="${FARCASTER_OIDC_USER_CLAIM:-sub}" \
  exec go run ./cmd/farcaster
