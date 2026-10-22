# 10,000 TPS 24-hour soak test

The blueprint's last load gate: the platform carries 10,000 TPS for 24 hours with nothing growing that should stay flat (memory, goroutines, the backlog of messages in flight), no service restarting, no submit refused, and money and history exact at the end. The 5,000 TPS sign-off (`docs/signoff-5k.md`) proves capacity for 30 minutes; the soak proves the platform does not degrade over a day.

## Where to run it

- **Layout:** the HA layout (`docs/phase5-ha.md`, `deploy/ha/`), sized as the blueprint says for 10,000 TPS, with Prometheus and Grafana on `MON_1` (`docs/signoff-5k.md`, step 8). The single-node sign-off layout cannot carry 10,000 TPS for a day.
- **Disk:** history grows by about 45 GB a day at 5,000 TPS, so ClickHouse needs about 100 GB free per replica. Redpanda keeps 72 h at three copies.
- **Load generator:** `SIM_1` runs the simulators and the load generator; give it 16 vCPUs at 10,000 TPS.
- **Balance:** 24 h at 10,000 TPS is 864 million messages. At the seed's ₹0.12 per message that is about ₹10.4 crore, so seed with `SEED_BALANCE=150000000` in `.env` before `--profile init run seed`, or top the demo account up. Otherwise the run stops with `insufficient_balance` refusals partway through, which is what happened on the first rehearsal.

## Run it

On `SIM_1`, detached (it runs 24 hours plus a 15-minute drain):

```sh
nohup scripts/signoff.sh soak > deploy/vm/reports/soak.log 2>&1 &
tail -f deploy/vm/reports/soak.log
```

The load generator (`smpp-load`) for the soak:
- **Load:** sends at `SOAK_TPS` (10,000) for `SOAK_DURATION` (24h) over 16 binds, re-binding any session that drops (`-reconnect`).
- **Hourly checkpoints:** writes one every hour to the report (`deploy/vm/reports/soak.json`): accepted TPS, refusals, p50 and p99 for that hour, receipts and messages still waiting for one.
- **Memory:** latency is kept in a fixed-size histogram, so the generator itself stays at a few MB for any run length.

It fails when:
- any submit is refused, or accepted TPS is below 99% of the target;
- any hour's p99 is over `SOAK_MAX_P99` (250 ms);
- more than `SOAK_MAX_OUTSTANDING` (200,000) messages wait for a receipt at a checkpoint;
- receipts are still missing 15 minutes after sending stops.

Then, on an app VM, with the start time the script printed:

```sh
scripts/signoff.sh soak-report <since>   # leaks, restarts, backlog (from Prometheus)
scripts/signoff.sh billing <since>       # money and history exact
```

`soak-report` compares the hour after warm-up with the last hour. It fails when:
- **memory:** a service's resident memory grew more than 25% (and by more than 64 MiB);
- **goroutines:** a service's goroutines grew more than 50% (and by more than 200);
- **backlog:** messages in flight (accepted and not yet final) grew more than 50% (and by more than 10,000);
- **restarts:** any service restarted;
- **refusals and drops:** any submit was refused or any history row dropped.

It also reports consumer batches retried and the overall rate. Flags change the limits (`soak-report -h`).

## Record the result

| Item | Value |
| --- | --- |
| Date, git commit, VM types | |
| Accepted TPS over 24 h, refusals | `soak.json` |
| Worst hourly p99, receipts outstanding at the worst checkpoint | `soak.json` intervals |
| Memory and goroutines, first hour against last, per service | `soak-report.json` |
| Messages in flight, first hour against last | `soak-report.json` |
| Restarts, history rows dropped | `soak-report.json` |
| billing-check | PASS / FAIL |

## Rehearsal on one machine

The tooling was rehearsed on the dev stack (`scripts/dev-local.sh`, in-memory Kafka) on one 4-vCPU machine. That machine cannot hold a day of traffic: the in-memory Kafka keeps every record. The result is in `docs/phase5-security.md` (Verification).
