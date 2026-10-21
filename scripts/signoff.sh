#!/usr/bin/env bash
# 5,000 TPS sign-off driver. See docs/signoff-5k.md for the VM layout and
# the full procedure.
#
#   scripts/signoff.sh render-monitoring   # on MON_1: write Prometheus config from deploy/vm/.env
#   scripts/signoff.sh run                 # on SIM_1: smoke, 30 min at 5,000 TPS, 5 min at 10,000 TPS
#   scripts/signoff.sh billing <since>     # on an APP VM: ledger / Redis / ClickHouse cross-check
#
# Thresholds (override with env): MAX_P99 (steady), BURST_MAX_P99, MAX_LAG (receipt lag).
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
VM=$ROOT/deploy/vm
ENV_FILE=$VM/.env
[ -f "$ENV_FILE" ] || { echo "missing $ENV_FILE (copy deploy/vm/env.example)" >&2; exit 1; }
set -a; . "$ENV_FILE"; set +a

STEADY_TPS=${STEADY_TPS:-5000}
STEADY_DURATION=${STEADY_DURATION:-30m}
BURST_TPS=${BURST_TPS:-10000}
BURST_DURATION=${BURST_DURATION:-5m}
MAX_P99=${MAX_P99:-100ms}
BURST_MAX_P99=${BURST_MAX_P99:-250ms}
MAX_LAG=${MAX_LAG:-60s}
BINDS=${BINDS:-16}

loadgen() {
  docker compose -f "$VM/sim.yml" --env-file "$ENV_FILE" --profile load run --rm loadgen \
    -addr "$APP_1:2775,$APP_2:2775" -binds "$BINDS" -window 200 -progress 30s "$@"
}

stage() { # name, then smpp-load flags
  local name=$1; shift
  echo "=== $name ($(date -u +%FT%TZ))"
  if loadgen -report "/reports/$name.json" "$@"; then
    echo "=== $name: PASS"
  else
    echo "=== $name: FAIL (see deploy/vm/reports/$name.json)"
    return 1
  fi
}

case "${1:-}" in
render-monitoring)
  join() { local port=$1; shift; local out=""; for h in "$@"; do out+="'$h:$port', "; done; echo "${out%, }"; }
  app=""
  for p in 9101 9102 9103 9104 9105 9106 9107 9108 9109 9110 9111; do app+="$(join $p "$APP_1" "$APP_2"), "; done
  sed -e "s|__APP_TARGETS__|${app%, }|" \
      -e "s|__KAFKA_TARGETS__|$(join 9644 "$KAFKA_1" "$KAFKA_2" "$KAFKA_3")|" \
      -e "s|__NODE_TARGETS__|$(join 9100 "$KAFKA_1" "$KAFKA_2" "$KAFKA_3" "$DATA_1" "$CH_1" "$APP_1" "$APP_2" "$SIM_1")|" \
      "$VM/monitoring/prometheus.yml.tmpl" > "$VM/monitoring/prometheus.yml"
  echo "wrote $VM/monitoring/prometheus.yml"
  ;;
run)
  mkdir -p "$VM/reports"
  SINCE=$(date -u +%FT%TZ)
  echo "sign-off started $SINCE; billing window starts here"
  echo "$SINCE" > "$VM/reports/since.txt"
  stage smoke  -tps 500 -duration 2m -drain 60s -max-failed 0 -min-tps-ratio 0.99 -max-receipt-lag 30s
  stage steady -tps "$STEADY_TPS" -duration "$STEADY_DURATION" -drain 5m \
    -max-failed 0 -min-tps-ratio 0.99 -max-p99 "$MAX_P99" -max-receipt-lag "$MAX_LAG"
  stage burst  -tps "$BURST_TPS" -duration "$BURST_DURATION" -drain 10m \
    -max-failed 0 -min-tps-ratio 0.98 -max-p99 "$BURST_MAX_P99" -max-receipt-lag 5m
  echo
  echo "All load stages passed. Next, on an APP VM:"
  echo "  scripts/signoff.sh billing $SINCE"
  ;;
billing)
  SINCE=${2:?usage: signoff.sh billing <since RFC3339>}
  docker compose -f "$VM/app.yml" --env-file "$ENV_FILE" --profile init run --rm \
    billing-check -since "$SINCE" -settle 5m
  ;;
*)
  sed -n '2,12p' "$0"
  exit 2
  ;;
esac
