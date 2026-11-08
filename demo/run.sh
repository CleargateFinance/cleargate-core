#!/usr/bin/env bash
# Runs Cleargate's own processes for the demo, with the repository's .env
# exported to them. Run each in its own terminal:
#
#   ./run.sh deps     the demo's Postgres and Redis in Docker, then the migrations
#   ./run.sh api      the API on :8080, its log also written to .state/api.log
#   ./run.sh worker   the worker: credits deposits, settles payments
#   ./run.sh reset    delete the demo's database and state, to start over
#
# The API's log carries the email verification link (MAIL_TRANSPORT=log), and
# `go run ./console setup` reads it from .state/api.log.
set -euo pipefail

demo="$(cd "$(dirname "$0")" && pwd)"
root="$(dirname "$demo")"
cd "$root"

if [[ ! -f .env ]]; then
  echo "No .env in $root: copy .env.example to .env and fill in CRYPTO_MASTER_KEYS and the CDP_* keys." >&2
  exit 1
fi
set -a
# shellcheck disable=SC1091
source ./.env
set +a

# The demo always uses its own containers (docker-compose.yml here), and runs
# in debug mode, the only mode in which the API will log emails.
export DATABASE_DSN="postgres://cleargate:cleargate@localhost:5433/cleargate?sslmode=disable"
export CACHE_ADDR="localhost:6381"
export SERVER_MODE=debug
export MAIL_TRANSPORT=log
export LOG_FORMAT=text

compose=(docker compose -f "$demo/docker-compose.yml")
mkdir -p "$demo/.state"

# The master key seals what the demo's database holds (TOTP secrets), so it
# lives and dies with that database rather than coming from .env.
if [[ ! -f "$demo/.state/master.key" ]]; then
  (umask 077 && openssl rand -base64 32 > "$demo/.state/master.key")
fi
export CRYPTO_MASTER_KEYS="1:$(cat "$demo/.state/master.key")"
export CRYPTO_ACTIVE_KEY_ID=1

case "${1:-}" in
  deps)
    "${compose[@]}" up -d --wait
    go run ./cmd/migrate up
    ;;
  api)
    go run ./cmd/api 2>&1 | tee -a "$demo/.state/api.log"
    ;;
  worker)
    go run ./cmd/worker
    ;;
  reset)
    "${compose[@]}" down -v
    # Keep the merchant's wallet: it holds what the demo earned, and its USDC
    # account already exists.
    find "$demo/.state" -mindepth 1 ! -name merchant.json -delete
    echo "Database and console state deleted; merchant wallet kept. Next: ./run.sh deps"
    ;;
  *)
    echo "usage: ./run.sh deps|api|worker|reset" >&2
    exit 2
    ;;
esac
