# Reset-weighted routing

This fork adds `routing.strategy: reset-weighted`. Enable `session-affinity` to
assign whole sessions rather than individual requests:

```yaml
routing:
  strategy: reset-weighted
  session-affinity: true
  session-affinity-ttl: 1h
  session-affinity-subagents: true
```

Account priority keeps upstream semantics: **higher numbers win**. For example,
set two credentials to priority `10` and a backup to `0`. New sessions share the
available priority-10 accounts; priority 0 is used when that group is unavailable
for the requested model. A session already using the backup stays there when a
higher-priority account recovers. Affinity expires after one hour of inactivity
and is held in memory, so restarting the server clears the bindings.

Within the chosen group, the scheduler uses smooth weighted round-robin. For
Codex it reads the latest account/model quota snapshot from upstream HTTP headers
or WebSocket quota events. The next future reset among the base primary and
secondary windows determines the weight. Each known account gets
`max(1, round(8 * soonest_remaining / account_remaining))`, so resets in one and
two hours produce weights 8 and 4. Remaining times below one minute are clamped
to one minute. Exhausted windows do not earn a reset preference; normal provider
errors and cooldown handling determine account availability.

Missing, malformed, expired, future-dated, or older-than-15-minute observations
receive weight 1. With no useful observations, selection is equal. Every eligible
account retains a share to obtain fresh observations. No background quota polling
is added. Reset observations and scheduling state are in memory. Additional
model-specific/reserve quota windows are not used for weighting. Other providers
receive equal shares within their preferred tier.

The weights are temporary scheduler inputs. They do not overwrite credential
metadata. The manually configured `weight` field belongs to the separate
`weighted-round-robin` strategy and is not used by `reset-weighted`; disable an
account explicitly to exclude it.

Configure priority through the existing credential editor or
`PATCH /v8/management/credentials/fields` with `name` and `priority`. For Codex
WebSocket upstream transport, set `websockets: true` on each credential using the
same endpoint. Both the client connection and credential must enable WebSockets;
ordinary HTTP clients continue to use HTTP upstream. Experimental full-duplex
`response-steering` is a separate setting and remains off.

Set the custom strategy in the YAML config editor or through the v8 config API.
The upstream visual strategy dropdown does not include this fork's option.
