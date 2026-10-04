# Traffic reporting compatibility

- Successful reports subtract only the acknowledged byte snapshot. Traffic received while HTTP is pending remains in the live counters.
- Failed reports leave counters unchanged. The next reporting cycle takes a fresh snapshot, including bytes received since the failed attempt.
- Changing speed/device limits or credentials without changing UID/email keeps the existing counters. Truly deleted users still discard failed reports and unregister their counters.
- SSPanel's existing `user_id`, `u`, and `d` format and HTTP retry behavior remain unchanged. No panel, SQL or YAML changes are required.
- Reports have no idempotency key. If the panel commits a report but its response is lost, retrying can still charge it again; this remains the legacy behavior.
- Like core counters, unreported traffic is in memory and does not survive process restart.

Regression check: `go test ./service/controller -run '^TestTraffic'`.
