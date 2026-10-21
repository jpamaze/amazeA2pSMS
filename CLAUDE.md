# Notes for Claude sessions

- Design source of truth: the "New SMS Platform Blueprint" doc (https://claude.ai/artifact/RDYoq6GNjNSiNxBPLDJWCj) and `docs/`.
- Launch scope: India only with full DLT; HTTP and SMPP for both customers and vendors; no postpaid. Wallet + credit limit (default 0); resellers set sub-account limits within their own available balance.
- Delivery when the session cannot push: commit on `main`, then `git bundle create <scratchpad>/amazeA2pSMS.bundle HEAD main` (include HEAD) and send the file. The user saves it as `D:\ZAPIM\SMPP\amazeA2pSMS.bundle` and applies it to `D:\ZAPIM\SMPP\amazeA2pSMS` with `scripts\sync-from-bundle.ps1`. Give Windows/PowerShell commands with those paths.
- Commit author: Jaiprakash Khowal <jp@amazesoftwaretech.com> (set with `git config user.name` / `user.email` before committing).
- Tests: `go test -race ./...` needs Redis on localhost:6379 and Postgres `sms_test` (wiped each run); they skip when unreachable.
- Full stack without Docker: `scripts/dev-local.sh up|load|down` (in-memory Kafka via `cmd/devkafka`, vendor `cmd/smpp-sim`). Phase status and test results: `docs/phase1-core-messaging.md`, `docs/phase2-billing.md`, `docs/phase2-deals-disputes.md`, `docs/phase3-dlt.md`, `docs/phase3-network.md`, `docs/phase4-portals.md`.
- Portal: `portal/` (Next.js static export). Build with `cd portal && npm ci && npm run build` before `dev-local.sh up` so admin-api serves it on :8081. Check UI changes with a headless Playwright walkthrough (install playwright in the scratchpad, not the repo; Chromium is at /opt/pw-browsers) and look at the screenshots.
- Dev portal users (devseed): ops@amaze.local (platform), owner@demo-reseller.local (reseller, brand BlueText SMS), owner@reseller-shop.local (its sub-account), owner@demo-customer.local (direct customer).
- Dev DLT data: devseed registers PE 1101234567890123456, header AMZTST and templates `Your OTP is {#var#}. Do not share it with anyone. -Amaze` (smpp-load's default) and `Test message {#var#}`; any other text to the demo customer is refused (dlt_template).
- Kill dev processes by exact PID (`pgrep -x name`), never `pkill -f <pattern>`: the pattern matches the calling shell and kills it.
- Start Redis outside the repo (`cd /tmp && redis-server --daemonize yes`) so dump.rdb is not written here.
- Before building a bundle, `git fetch origin main` and build on top of it so the user's pull fast-forwards.
