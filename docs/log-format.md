# Compact XrayR logs

Xray core logs use timestamps with second precision and omit the session ID from
all log levels. This applies to stdout, stderr, access logs and error logs. Log
levels and network addresses remain unchanged. Internal routing tags are unchanged. The core
still generates internal session IDs; only their log prefix is removed.

For routed access logs, XrayR's `inboundTag|email|uid` user identity is displayed
as `email:email|uid` when the prefix matches the logged inbound tag. The stored
identity is unchanged, so authentication, traffic counters and limits still use
the complete identity. Plain custom-inbound emails remain unchanged. Both routing
tags in `[inbound >> outbound]` are retained, but the `_0.0.0.0_` segment is
displayed as `_`, for example `[Vless_36361 >> Vless_36361]`. This only changes
access-log display, not inbound/outbound definitions or routing rules. Other
listen IPs and custom outbound tags such as `IPv6_out` and `Socks1` are unchanged.

XrayR's own controller and API logs use the same `YYYY/MM/DD HH:MM:SS [Level]`
prefix, without fractional seconds, a timezone suffix, or `time=`, `level=` and
`msg=` labels. Complete node context is appended as `Host|ID|Type`, without field
names or quotes, for example `https://panel.example|255|Vless`. Other context
fields remain available. The node `ID` is not a session ID or process PID.

Example after deploying the rebuilt XrayR binary:

```text
2026/10/04 11:26:02 [Debug] proxy: XtlsPadding 1201 28 2
2026/10/04 11:28:43 [Info] Added 1 new users https://panel.example|255|Vless
2026/10/04 11:28:43 from 127.0.0.1:12345 accepted tcp:example.com:443 [Vless_36361 >> Vless_36361] email:user@example.com|2
```

The `Oct 04 ... hostname XrayR[PID]:` prefix is journalctl metadata, not text emitted
by XrayR. Hide that display prefix with the native message-only output mode:

```sh
journalctl -u XrayR.service -n 100 -f -o cat --no-pager
```

This keeps the timestamp in the application message and does not change the
system journal, delete recorded PID metadata or rewrite old logs. If an external
`xrayr log` script uses journalctl, its journalctl invocation needs `-o cat` too;
that installed script is not part of this repository. No YAML changes are needed.

Regression check from `third_party/xray-core`:

```sh
go test ./common/log ./common/errors ./app/log
```

Controller format check from the repository root: `go test ./cmd -run '^TestPlainTextLogFormat$'`.
