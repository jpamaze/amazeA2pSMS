#!/usr/bin/env bash
# 5,000 TPS sign-off driver. See docs/signoff-5k.md for the VM layout and
# the full procedure.
#
#   scripts/signoff.sh render-monitoring   # on MON_1: write Prometheus config from deploy/vm/.env
#   scripts/signoff.sh run                 # on SIM_1: smoke, 30 min at 5,000 TPS, 5 min at 10,000 TPS
#   scripts/signoff.sh billing <since>     # on an APP VM: ledger / Redis / ClickHouse cross-check
#   scripts/signoff.sh soak                # on SIM_1: 10,000 TPS for 24 h (docs/soak-test.md)
#   scripts/signoff.sh soak-report <since> # leak / restart / backlog checks from Prometheus
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
SOAK_TPS=${SOAK_TPS:-10000}
SOAK_DURATION=${SOAK_DURATION:-24h}
SOAK_MAX_P99=${SOAK_MAX_P99:-250ms}
SOAK_MAX_OUTSTANDING=${SOAK_MAX_OUTSTANDING:-200000}

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
  # Targets for every VM named in .env: the sign-off layout (APP_1/2, DATA_1,
  # CH_1) or the HA layout (APP_1..3, PG_*, REDIS_*, CH_*, LB_*).
  shopt -u patsub_replacement 2>/dev/null || true  # "&" in a value stays "&"
  join() { local port=$1 out="" h; shift; for h in "$@"; do if [ -n "$h" ]; then out+="'$h:$port', "; fi; done; echo "${out%, }"; }
  apps=("$APP_1" "$APP_2" "${APP_3:-}")
  app=""
  for p in 9101 9102 9103 9104 9105 9106 9107 9109 9110 9111; do app+="$(join $p "${apps[@]}"), "; done
  pgs=("${PG_1:-}" "${PG_2:-}" "${PG_3:-}") rds=("${REDIS_1:-}" "${REDIS_2:-}" "${REDIS_3:-}")
  chs=("${CH_1:-}" "${CH_2:-}" "${CH_3:-}") lbs=("${LB_1:-}" "${LB_2:-}")
  nodes=("$KAFKA_1" "$KAFKA_2" "$KAFKA_3" "${apps[@]}" "${SIM_1:-}" "${MON_1:-}" "${lbs[@]}" "${rds[@]}" "${chs[@]}" "${pgs[@]}")
  if [ -z "${PG_1:-}" ]; then nodes+=("${DATA_1:-}"); fi  # sign-off layout: PostgreSQL and Redis on DATA_1
  t=$(<"$VM/monitoring/prometheus.yml.tmpl")
  t=${t//__APP_TARGETS__/${app%, }}
  t=${t//__KAFKA_TARGETS__/$(join 9644 "$KAFKA_1" "$KAFKA_2" "$KAFKA_3")}
  t=${t//__NODE_TARGETS__/$(join 9100 $(printf '%s\n' "${nodes[@]}" | awk 'NF && !seen[$0]++'))}
  t=${t//__PATRONI_TARGETS__/$(join 8008 "${pgs[@]}")}
  t=${t//__REDIS_TARGETS__/$(join 9121 "${rds[@]}")}
  ch=""; if [ -n "${PG_1:-}" ]; then ch=$(join 9363 "${chs[@]}"); fi  # HA ClickHouse only
  t=${t//__CH_TARGETS__/$ch}
  t=${t//__LB_TARGETS__/$(join 7001 "${lbs[@]}")}
  printf '%s\n' "$t" > "$VM/monitoring/prometheus.yml"
  q() { local v=${1-}; printf '%s' "${v//\'/\'\'}"; }  # inside single quotes in YAML
  a=$(<"$VM/monitoring/alertmanager.yml.tmpl")
  a=${a//__SMTP_HOST__/$(q "${SMTP_HOST:-localhost}")}
  a=${a//__SMTP_PORT__/$(q "${SMTP_PORT:-587}")}
  a=${a//__SMTP_FROM__/$(q "${SMTP_FROM:-alerts@example.com}")}
  a=${a//__SMTP_USER__/$(q "${SMTP_USER:-}")}
  a=${a//__SMTP_PASSWORD__/$(q "${SMTP_PASSWORD:-}")}
  a=${a//__SMTP_REQUIRE_TLS__/$( [ "${SMTP_TLS:-starttls}" = starttls ] && echo true || echo false)}
  a=${a//__ALERT_EMAIL_TO__/$(q "${ALERT_EMAIL_TO:-ops@example.com}")}
  hook=""
  if [ -n "${ALERT_WEBHOOK_URL:-}" ]; then hook="    webhook_configs:
      - url: '$(q "$ALERT_WEBHOOK_URL")'
        send_resolved: true"; fi
  a=${a//__WEBHOOK__/$hook}
  printf '%s\n' "$a" > "$VM/monitoring/alertmanager.yml"
  # Holds the SMTP password: readable only by the container's user (nobody).
  if [ "$(id -u)" = 0 ]; then chown 65534:65534 "$VM/monitoring/alertmanager.yml"; chmod 600 "$VM/monitoring/alertmanager.yml"
  else chmod 644 "$VM/monitoring/alertmanager.yml"; echo "note: run as root to make alertmanager.yml readable by its container only"; fi
  echo "wrote $VM/monitoring/prometheus.yml and alertmanager.yml"
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
soak)
  # Run it detached (nohup or tmux): it takes SOAK_DURATION plus the drain.
  mkdir -p "$VM/reports"
  SINCE=$(date -u +%FT%TZ)
  echo "soak started $SINCE"
  echo "$SINCE" > "$VM/reports/soak-since.txt"
  stage soak -tps "$SOAK_TPS" -duration "$SOAK_DURATION" -drain 15m -interval 1h -reconnect \
    -max-failed 0 -min-tps-ratio 0.99 -max-p99 "$SOAK_MAX_P99" -max-receipt-lag 5m -max-outstanding "$SOAK_MAX_OUTSTANDING"
  echo
  echo "Load passed. Next, on an APP VM:"
  echo "  scripts/signoff.sh soak-report $SINCE"
  echo "  scripts/signoff.sh billing $SINCE"
  ;;
soak-report)
  SINCE=${2:?usage: signoff.sh soak-report <since RFC3339>}
  docker compose -f "$VM/app.yml" --env-file "$ENV_FILE" --profile ops run --rm \
    -v "$VM/reports:/reports" soak-report -prometheus "http://$MON_1:9090" -start "$SINCE" -report /reports/soak-report.json
  ;;
billing)
  SINCE=${2:?usage: signoff.sh billing <since RFC3339>}
  docker compose -f "$VM/app.yml" --env-file "$ENV_FILE" --profile init run --rm \
    billing-check -since "$SINCE" -settle 5m
  ;;
*)
  sed -n '2,14p' "$0"
  exit 2
  ;;
esac
