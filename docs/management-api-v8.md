# v8 Management API

The base path is `/v8/management`. Configuration endpoints follow the structure in
[config.example.yaml](../config.example.yaml); operational endpoints are grouped
by function. The existing `/v0/management` API remains supported. Its reference is
available in the [management API guide](https://help.router-for.me/management/api).

## Access

The v8 endpoints use the same management key and remote-access policy as v0.
Send `Authorization: Bearer <management-key>` or `X-Management-Key: <management-key>`.
OAuth callbacks validate a pending login state and do not require a management-key
header. Home mode disables local management endpoints for both versions.

## Configuration endpoints

All paths below are relative to `/v8/management`.

| Path | Methods | Description |
| --- | --- | --- |
| `/config` | GET, PUT, PATCH | Read, replace, or merge the complete configuration as JSON. |
| `/config.yaml` | GET, PUT | Read or replace the complete configuration as YAML. |
| `/config/<section>/<field>` | GET, PUT, PATCH, DELETE | Read or modify a section or field using its path in the v8 tree. |

GET renders the persisted configuration in the v8 layout without migrating the
file or adding omitted runtime defaults. Successful v8 configuration writes save
the latest layout; reads and rejected writes leave the file unchanged.
Historical v8 paths and request bodies remain accepted as aliases.

JSON writes accept the value directly, without a `{ "value": ... }` envelope.
PUT replaces its target. PATCH merges objects and replaces lists and scalars.
Scalar field updates retain existing comments; complete replacements use the submitted document.
DELETE removes a field. Paths identify mapping keys, not array indexes; replace
an entire list to change its entries. Optional per-key `null` overrides inherit
the corresponding group value. Legacy field names are rejected by v8 writes.

| Example path | Value or purpose |
| --- | --- |
| `/config/access/api-keys` | Client authentication keys, for example `["client-key"]`. |
| `/config/access/api-key-names` | Optional client key display names for usage reports, for example `{"client-key": "MacBook"}`. |
| `/config/access/api-key-limits` | Optional Claude allowance per client key in Pro units per weekly window, for example `{"client-key": 1.5}`; `0` means no limit. |
| `/config/api-keys` | All upstream provider groups. |
| `/config/api-keys/codex` | Codex upstream groups. |
| `/config/client/codex/optimize-multi-agent-v2` | Boolean, default `false`; applies to Codex clients across OAuth and API-key routes. |
| `/config/observability/logs/debug` | A boolean, for example `true`. |
| `/config/routing/retry/request-retry` | A number, for example `0`. |
| `/config/plugins/configs/<id>` | A plugin configuration object. |

For example, `PATCH /v8/management/config` with this body disables additional
request retries while preserving other settings:

```json
{
  "routing": {
    "retry": {
      "request-retry": 0
    }
  }
}
```

JSON reads omit TURN usernames and credentials under
`oauth.providers.codex.live-media-relay.ice-servers`; YAML reads include them.
The Home-owned revision fields
`credentials.concurrency.lifecycle-config-revision`,
`credentials.concurrency.observation-barrier-revision`, and `plugins.auth-revision`
cannot be changed through these endpoints.

### Codex multi-agent configuration migration

`client.codex.optimize-multi-agent-v2` is the sole runtime setting. Loading older
YAML accepts `providers.codex.optimize-multi-agent-v2`,
`oauth.providers.codex.optimize-multi-agent-v2`, and the flat
`codex.optimize-multi-agent-v2` path. The client path always wins conflicts,
including explicit `false` or `null`. If it is absent, the historical OAuth path
wins over `providers`, which wins over the flat path.

These aliases now have client-wide semantics, not OAuth-only scope. Loading does
not rewrite legacy-only settings; normalization removes aliases conflicting with
the client path. A successful v8 write migrates the aliases while preserving their
comments. Historical API paths remain usable. Home configuration publishers
need matching client schema support before emitting the new path; older YAML
payloads remain readable.

## Operational endpoints

All paths below are relative to `/v8/management`. Request and response bodies
retain the corresponding business operation's fields.

| Path | Methods | Description |
| --- | --- | --- |
| `/config/upstream/<provider>` | GET, PUT, PATCH, DELETE | Manage shared provider settings. |
| `/server/latest-version` | GET | Get latest release information. |
| `/requests/api-call` | POST | Make an authenticated upstream call. |
| `/routing/cooldown/reset` | POST | Clear credential cooldown. |
| `/routing/model-definitions/<channel>` | GET | Get model definitions. |
| `/observability/logs` | GET, DELETE | Read or clear application logs. |
| `/observability/logs/errors` | GET | List error-log files. |
| `/observability/logs/errors/<name>` | GET | Download an error-log file. |
| `/observability/logs/requests/<id>` | GET | Get a request log. |
| `/observability/usage/api-keys` | GET | Get upstream provider API-key usage. |
| `/observability/usage/clients` | GET, DELETE | Get or reset usage per client API key (`access.api-keys`). |
| `/observability/usage/clients/window/reset` | POST | End a client key's current Claude allowance window, keeping its history. |
| `/observability/usage/queue` | GET | Get queued usage events. |
| `/observability/routing` | GET | Get live routing state and recent credential selections. |
| `/credentials` | GET, POST, DELETE | List, upload, or delete credential files. |
| `/credentials/models` | GET | Get credential models. |
| `/credentials/download` | GET | Download a credential file. |
| `/credentials/status` | PATCH | Change credential status. |
| `/credentials/fields` | PATCH | Change credential fields. |
| `/credentials/refresh` | POST | Refresh credentials. |
| `/oauth/import?provider=vertex` | POST | Import a Vertex service account using a multipart `file` upload. |
| `/oauth/auth-url?provider=<provider>` | GET | Start built-in or plugin OAuth. |
| `/oauth/status?state=<state>` | GET | Get login status. |
| `/oauth/session?state=<state>` | DELETE | Cancel a login session. |
| `/oauth/callback` | GET, POST | Submit an OAuth callback. |
| `/plugins` | GET | List installed plugins. |
| `/plugins/<id>` | DELETE | Delete a plugin. |
| `/plugins/store` | GET | List the plugin store. |
| `/plugins/store/<id>/install` | POST | Install or update a plugin. |
| `/plugins/<id>/quota` | GET, POST, DELETE | Read, fetch, or reset plugin quota. |

## Routing observability

`GET /observability/routing` reports what the running selector is doing on this
node. State is in memory only and resets when the process restarts.

```json
{
  "observed_at": "2026-10-04T10:00:00Z",
  "since": "2026-10-04T08:00:00Z",
  "mode": "local",
  "strategy": "round-robin",
  "plugin_scheduler": false,
  "session_affinity": {
    "enabled": true,
    "ttl_seconds": 3600,
    "subagents": true,
    "active_sessions": 4,
    "sessions_by_auth_index": { "a1b2c3d4e5f6a7b8": 3, "0f1e2d3c4b5a6978": 1 }
  },
  "counters": {
    "selections": 120, "retries": 1, "failovers": 3,
    "affinity_hits": 90, "affinity_new": 25, "affinity_rebinds": 2,
    "transport_websocket": 40, "transport_http": 10
  },
  "recent": [
    {
      "time": "2026-10-04T09:59:58Z",
      "provider": "codex",
      "model": "gpt-5",
      "auth_index": "a1b2c3d4e5f6a7b8",
      "selection": "affinity_rebind",
      "candidates": 2,
      "attempt": 1,
      "attempt_kind": "initial",
      "previous_auth_index": "0f1e2d3c4b5a6978",
      "session": "9c1e44d0",
      "transport": "websocket"
    }
  ]
}
```

- `mode` is `home` when selection is delegated to CLIProxyAPIHome; local
  decisions and counters are then not recorded.
- `strategy` is the effective strategy of the running selector (`round-robin`,
  `weighted-round-robin`, `fill-first`, `quota-aware`, or `custom`), after
  aliases and unknown values are normalized.
- `selection` is `strategy`, `affinity_hit`, `affinity_new`, `affinity_rebind`
  (the bound credential was unavailable and the session moved), `pinned`, or
  `plugin`. `strategy_reason` carries the quota-aware decision reason.
- `candidates` is the number of ready credentials the selector chose among,
  after cooldown, priority, and per-request exclusions.
- `attempt_kind` is `initial`, `retry` (same credential picked again in a
  later retry round), or `failover` (a different credential after a failed
  attempt in the same request).
- `session` is a short hash for correlating decisions; raw session IDs are not
  exposed. `transport` is set for Codex attempts (`websocket` or `http`).
- `recent` holds the latest 100 selections, newest first.

## Client API key usage

`GET /observability/usage/clients` reports how much each client API key (for
example, one key per device) has used the proxy. Raw keys are never returned or
stored: each key is identified by `id`, the first 16 hex characters of its
SHA-256, plus a masked `key` for keys still listed in `access.api-keys`. Add
`access.api-key-names` to show a display `name`; names may be keyed by the full
API key or by its `id`. Requests without a client key (when `access.api-keys` is
empty) are grouped under the `anonymous` id. Add `access.api-key-limits` to cap
a key's Claude usage per 7-day window (see "Claude allowance per client key"
below).

Usage is saved to `client-usage.json` next to the config file (or under
`WRITABLE_PATH`) every minute and on shutdown, and restored on startup. A file that
cannot be read as usage state is renamed to `client-usage.json.invalid-<unix nanoseconds>`
and tracking starts fresh. Records still queued for the usage plugins when the
process stops may be lost. Usage is not tracked in Home mode.

```json
{
  "generated_at": "2026-10-07T12:00:00Z",
  "since": "2026-10-01T08:00:00Z",
  "claude_limits_supported": true,
  "keys": [
    {
      "id": "3f9a1c2b7d4e5f60",
      "name": "MacBook",
      "key": "sk-m...9f3k",
      "configured": true,
      "first_used_at": "2026-10-01T08:05:00Z",
      "last_used_at": "2026-10-07T11:59:00Z",
      "totals": {
        "requests": 412, "failed": 6, "blocked": 3,
        "tokens": {
          "input_tokens": 9100000, "output_tokens": 310000, "reasoning_tokens": 42000,
          "cache_read_tokens": 8200000, "cache_write_tokens": 600000, "total_tokens": 9410000
        }
      },
      "models": { "claude-sonnet-4-5": { "requests": 400, "failed": 5, "tokens": { "total_tokens": 9300000 } } },
      "daily": [ { "date": "2026-10-07", "requests": 51, "failed": 0, "blocked": 3, "tokens": { "total_tokens": 1200000 } } ],
      "claude": {
        "current_pro_units": 0.84,
        "total_pro_units": 2.31,
        "window_started_at": "2026-10-03T09:12:00Z",
        "window_resets_at": "2026-10-10T09:12:00Z",
        "limit_pro_units": 1.5,
        "remaining_pro_units": 0.66,
        "limit_reached": false,
        "limit_resets_at": "2026-10-10T09:12:00Z",
        "credentials": [
          {
            "auth_id": "claude-user@example.com.json",
            "auth_index": "a1b2c3d4e5f6a7b8",
            "label": "user@example.com",
            "plan": "max_5x",
            "plan_pro_units": 2,
            "plan_source": "rate_limit_tier",
            "current_fraction": 0.42,
            "current_pro_units": 0.84,
            "total_fraction": 1.155,
            "total_pro_units": 2.31
          }
        ]
      }
    }
  ],
  "claude_credentials": [
    {
      "auth_id": "claude-user@example.com.json",
      "auth_index": "a1b2c3d4e5f6a7b8",
      "label": "user@example.com",
      "plan": "max_5x",
      "plan_pro_units": 2,
      "plan_source": "rate_limit_tier",
      "weekly_utilization": 0.61,
      "window_resets_at": "2026-10-09T15:00:00Z",
      "observed_at": "2026-10-07T11:59:00Z",
      "unattributed_current_fraction": 0.03,
      "unattributed_total_fraction": 0.05
    }
  ]
}
```

- `requests` counts successful upstream responses, so a request retried on
  another credential counts once; `failed` counts failed upstream attempts,
  including ones that were retried. Token fields do not overlap except that
  `input_tokens` includes cache reads and writes, and `output_tokens` includes
  reasoning.
- `blocked` counts requests refused by the key's Claude allowance (see "Claude
  allowance per client key" below). Refused requests are not counted as
  `requests` or `failed`, and `models` entries have no `blocked` field.
- `daily` covers the last 31 days, by the server's local date.
- `claude` measures Claude subscription usage in Claude Pro units: `1.0` is one
  full weekly allowance of a Pro plan. A Team plan is worth 1.25 units, a Max 5x
  plan 2 units, and a Max 20x plan 10 units per week. `current_*` covers the
  key's own 7-day window, like a subscription period: it opens at the key's
  first Claude request, ends exactly seven days later, and the next window opens
  at the key's next Claude request after that, so an idle key has no running
  window and `current_*` is `0`. `window_started_at` and `window_resets_at` are
  present while a window is open. The weekly resets of the credentials the key
  uses do not touch it. `total_*` accumulates across windows. Usage is converted
  with the plan in effect when it was attributed, so a later plan change does not
  rewrite history; usage attributed while the plan was unknown uses the current
  plan. `claude` is present when the key has Claude usage, an open window or a
  configured limit. Each `credentials` entry gives the key's usage of that
  credential inside the window (`current_*`) and since tracking began (`total_*`).
- `limit_pro_units` and `remaining_pro_units` (`max(limit - current, 0)`) are
  present when the key has a configured, non-zero allowance. `limit_reached` is
  always present and `false` without a limit. It is `true` exactly when
  `current_pro_units`, as reported (rounded to six decimals), is at least
  `limit_pro_units`; admission uses the same comparison, so the proxy refuses
  the key's Claude requests exactly when the report says the limit is reached. `limit_resets_at` equals
  `window_resets_at` and is present whenever a window is open, with or without
  a limit.
- `claude_limits_supported` is `true` on backends that enforce
  `access.api-key-limits`; older backends omit it and the limit fields above.
- Keys with a configured limit are listed even without usage and even when they
  are not in `access.api-keys` (`configured` is then `false`).
- Claude credentials report weekly usage as a fraction of their own plan limit
  (`Anthropic-Ratelimit-Unified-7d-Utilization`). Each increase is split between
  the client keys that used the credential since the previous increase, by
  API-price-weighted tokens. Usage made outside the proxy (for example on
  claude.ai) before the next proxy response is therefore counted toward those
  keys; increases with no proxy usage pending, and usage of reset keys, are
  reported as `unattributed_*` on the credential. Pending usage from before a
  weekly reset is not charged for the new window. The first response after
  tracking starts is a baseline and is not attributed, and a drop in utilization
  (limits reset or rescaled) starts a new baseline.
- `plan` comes from the credential's `plan_type` field when set (`pro`, `team`,
  `max_5x`, or `max_20x`; set it with `PATCH /credentials/fields`), otherwise
  from the plan Claude reports at login and token refresh (`rate_limit_tier`,
  `organization_type`). Plans without a known allowance (for example
  `enterprise`) use the credential `weight` as their allowance, defaulting to 1.

`DELETE /observability/usage/clients?id=<id>` resets one key and returns 404
for an unknown id. `DELETE /observability/usage/clients?all=true` resets every
key; a request with neither parameter is rejected with 400.

`POST /observability/usage/clients/window/reset?id=<id>` ends one key's current
Claude allowance window without deleting anything else: its `current_*` usage
drops to `0`, it is admitted again, its next Claude request opens a fresh window,
and `totals`, `daily` and the per-credential `total_*` values are kept.
`?all=true` does this for every key; the same 404 and 400 rules apply. After
either reset, a request of the key that had started before the reset opens no
window when its response is processed: only a request started after the reset
does. Usage attributed to such a request, or to a request from the key's
previous window, after the next window opened counts in `total_*` only. A
window starts at the earliest request of its period even when responses
are processed out of order, and a window that had ended when the state was
saved stays over after a restart, whatever the clock says.

### Claude allowance per client key

`access.api-key-limits` caps each client key's Claude usage in Pro units per
7-day window. The window is the key's own, as described above: it opens at the
key's first Claude request, ends exactly seven days later, and the next one
opens at the key's next Claude request, so a key is never held to the reset
schedule of the credentials it happens to use. Entries are keyed by the full
API key or by its `id`; `0` or a missing entry means no limit. A full-key entry wins over an `id` entry, so a
full-key `0` lifts a limit set on the `id`. Requests without a client key use
the `anonymous` entry. An entry is read as an `id` when it is 16 lowercase hex
characters (or `anonymous`), unless it equals a key in `access.api-keys` or the
key it spells has usage; a real key that happens to look like an `id` is
therefore still reported under its own `id`, masked. When an entry names both
a key and another key's `id` (a client key equal to some other key's `id`), it
applies to both, as it does for admission. Keys with a limit are
tracked even when the usage tracker is at its capacity of 1024 keys. Limits take
effect on every configuration reload (file changes and management writes)
without a restart. Negative, NaN or infinite values and duplicated entries are
rejected: the file fails to load and management writes return
`400 invalid_config`; error messages mask the key (keys of four characters or
fewer are masked completely, in errors and in the report's `key` field).
Values must be plain scalars: a number, or empty for no limit (a quoted or
tagged value is rejected before the YAML decoder can print it). The map itself
must be a mapping or empty: a scalar or a list in its place, directly or
behind an alias, is rejected before decoding as well, and a conversion error
the decoder reports for the map is masked like one for a value. An entry's
text is always a client key, even `api-key-limits` or `api-key-names`. An
empty value written in a flow mapping (`{key: }`) is saved back as an explicit
`null`, so a re-encoded file keeps meaning "no limit". An
entry counts as duplicated within one mapping, wherever that mapping appears in
the document: under a YAML merge key (`<<`), behind an alias, in an anchored
copy that a direct entry shadows, or in a nested `PATCH` body. An entry set
directly still overrides the same entry from a merged mapping; an alias key
counts as the key it resolves to. Entries must be plain scalar keys: compound
keys and explicitly tagged keys are rejected. Any key, value or anchor name the
YAML parser or decoder itself reports (a duplicated or non-scalar key, a tagged
scalar its tag does not fit, an unknown or self-referencing anchor) is masked
the same way, with line numbers kept, as is the value in any conversion error
the decoder reports (``cannot unmarshal !!str `fi...ey` into int``). A client key
the document names in a client key map or in `access.api-keys` and aliases
elsewhere, as a section, provider or field name, is masked in the diagnostic
that names it and in the warning logged when a write comments out an unknown
section. Diagnostics name the map (`api-key-limits` or `api-key-names`) and the
line, never the keys above it.

When a key's `current_pro_units` reaches its limit, the proxy refuses that key's
requests to every Claude credential, OAuth and API-key alike, before any
upstream connection is made. Other providers keep serving the key, and
count-tokens requests are never refused. The refusal is `429 Too Many Requests`
with a `Retry-After: <seconds>` header (`ceil(limit_resets_at - now)`, at least
`1`; `60` when no reset time is known), sent regardless of
`passthrough-headers`. The body is the endpoint's usual error envelope.
OpenAI-style endpoints receive

```json
{"error":{"message":"client API key Claude allowance reached: 1.52 of 1.5 Pro units used in the current 7-day window; resets in 2d3h","type":"rate_limit_error","code":"rate_limit_exceeded"}}
```

the Claude messages endpoint Anthropic's native envelope

```json
{"type":"error","error":{"type":"rate_limit_error","message":"client API key Claude allowance reached: 1.52 of 1.5 Pro units used in the current 7-day window; resets in 2d3h"}}
```

and Gemini-style endpoints their usual shape with the same status and message.
The message never contains the raw key. Streaming requests receive the same
JSON body before any stream opens, never an SSE error event. On the
`/v1/responses` WebSocket the HTTP upgrade has already happened, so a refused
`response.create` is answered with an `error` event carrying the same status,
headers and envelope, after which the proxy closes the socket; reconnect after
`Retry-After`:

```json
{"type":"error","status":429,"headers":{"Content-Type":"application/json","Retry-After":"5400"},"error":{"message":"client API key Claude allowance reached: 1.52 of 1.5 Pro units used in the current 7-day window; resets in 1h30m","type":"rate_limit_error","code":"rate_limit_exceeded"}}
```

Refused requests
increment `blocked` only: they are not counted as `requests` or `failed`,
publish no usage record and do not cool down any credential. A request that is
refused on Claude but served by another provider is not blocked and is not
counted; `blocked` counts only requests the proxy answered with the 429. When a
request fails upstream on another provider and a retry round then finds only
the Claude refusal, the upstream failure is reported, not the 429. Admission is
decided once per client request: a stream bootstrap retry
(`streaming.bootstrap-retries`) of a request already admitted proceeds even if
that request's own first attempt reached the limit, and reports the upstream
outcome. A model call a plugin makes from inside a request is a request of its
own and is admitted on its own. A request whose admission could not be decided
(the policy failed while deciding) fails instead of being admitted; it never
tries another credential as if the policy had refused only one. With
`nonstream-keepalive-interval` set, no keepalive byte is written to a
non-streaming response of a key with an allowance before the request is
committed to an upstream attempt, so a refusal is still answered with the 429:
the first byte commits the response status. A model call a plugin makes
before that point does not release the keepalive either, whether a credential
or a plugin executor serves it, so a long one sends no keepalive bytes; that
cannot change without giving up the 429, because the request's own admission
is decided only after the plugin returns. Requests of keys without an
allowance, and every request in Home mode, get the keepalive from the start
(an allowance added for such a key while one of its requests waits is then
reported, if reached, after keepalive bytes in a `200` response). The image
generation stream's bootstrap heartbeat (`streaming.keepalive-seconds`) waits
for the same point, so a refused image request is a plain JSON 429 rather than
an SSE error.

A key is admitted again as soon as its current usage is below the limit: when
its window ends, when the limit is raised or removed, when
`POST /observability/usage/clients/window/reset?id=<id>` ends the window early
(keeping the key's history), or when `DELETE /observability/usage/clients?id=<id>`
resets the key.

Limitations: attribution is estimate-based (usage is split from the
`Anthropic-Ratelimit-Unified-7d-Utilization` header deltas as described above),
so a key's share is approximate; of a key's share of an increase, only the part
from its requests inside its window (by their API-price weight) counts toward
that window, and only while the window is open, so the increase carried by the
first request of a new window is left out of it (that usage predates the
request), and usage of requests from before the window that is attributed after
it opened counts in the totals only; requests already in flight when the limit
is reached complete and may overshoot it; usage not yet flushed at shutdown is
lost; and limits are not enforced in Home mode, where usage is not tracked.

## OAuth

The login URL is shared by all providers. Set the required `provider` query
parameter to `claude`, `codex`, `antigravity`, `kimi`, `kimi-ai`, `xai`, `devin`,
`meta`, or a registered plugin provider ID. For example:

```http
GET /v8/management/oauth/auth-url?provider=codex
Authorization: Bearer <management-key>
```

Keep any provider-specific login parameters in the query string. The login
response includes the authorization URL and a session `state`. Use that state
for status queries and cancellation.

Callbacks accept `provider`, `state`, `code`, and `error` as GET query parameters
or POST JSON fields. POST also accepts `redirect_url` containing the complete
callback URL. If `provider` is omitted, it is inferred from the pending state;
an explicit provider must match that state. Poll until the status is `ok` or
`error`; `wait` means the login is still pending. Callback acceptance alone does
not mean credential exchange and persistence have completed.

Import uses `POST /v8/management/oauth/import?provider=vertex`. The `provider`
query parameter is required; Vertex is currently the supported import provider.

## v0 compatibility

The `/v0/management` endpoints keep their original paths and payloads, including
provider-specific OAuth login URLs, callbacks, status queries, and cancellation.
For example, `GET /v0/management/codex-auth-url` and
`GET /v8/management/oauth/auth-url?provider=codex` start the same login flow.

Legacy flat configuration endpoints such as `/debug`, `/request-retry`, and
`/codex-api-key` exist under `/v0/management` only.
`/v0/management/api-keys` continues to manage client authentication keys; the v8
equivalent is `/v8/management/config/access/api-keys`.

Legacy-only configuration files keep their layout until a successful v8
configuration write. When both layouts specify a field, the new field takes
precedence and its legacy equivalent is removed. V0 setters keep legacy-only
files in their original layout; existing v8 files are saved in the latest v8
layout. `PUT /v0/management/config.yaml` still replaces the complete file and
accepts legacy, new, or mixed layouts, normalizing v8 documents on save.

Plugin OAuth uses the shared v8 login endpoint. Other plugin-defined HTTP
extensions retain their declared `/v0/management` routes.
