# cliproxy-provider-openrouter

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) provider plugin for
[OpenRouter](https://openrouter.ai). It is built on
[cliproxy-provider-kit](https://github.com/TogetherWeOwn/cliproxy-provider-kit)
(pinned at `21069bf`), targets CLIProxyAPI **v8.0.49** (plugin RPC schema 10),
and is tested against the real v8.0.49 plugin loader. It adds no scheduler, no
background poller and no host configuration.

What it does:

- reads plugin-owned auth files (`"type": "openrouter"`);
- registers OpenRouter's catalog under the fixed `openrouter/` prefix, so it never
  registers a bare `vendor/model` ID that could clash with an official provider;
- executes OpenAI chat completions on OpenRouter through the host HTTP client;
- refuses inference before dispatch when the key's credit limit (or any budget
  over it) is spent, has no limit, or cannot be read;
- shows the key limit, budgets, free-model requests and optional account balance
  as a quota card, with the right reset time for each window.

**Cutover is not yet recommended.** The behavior below is verified against the
OpenRouter documentation and mocks only; see [Pending live verification](#pending-live-verification).

## Warning: this spends real money

- OpenRouter bills real USD per request.
- **Always set a credit limit and a reset window on the key** at
  [openrouter.ai/settings/keys](https://openrouter.ai/settings/keys). OpenRouter
  enforces that limit server side; it is the hard stop.
- A key **without** a credit limit is refused (`403 key_limit_required`) unless
  its auth file sets `"allow_unlimited_key": true`.
- `min_remaining_usd` is a soft reserve. The plugin keeps no local spend tally:
  requests admitted within one 60-second key reading can overshoot it by their
  combined cost. The key limit still caps the total.
- Variable-price routers (pricing `"-1"`, for example `openrouter/auto`) are not
  registered, because their cost is unknowable before the call.

## Auth files

One JSON file per key, mode `0600`, in the CLIProxy auth directory. Never commit
one.

```json
{
  "type": "openrouter",
  "api_key": "<inference key>",
  "management_api_key": "<optional read-only management key>",
  "min_remaining_usd": 0.5,
  "allow_unlimited_key": false,
  "label": "OpenRouter main"
}
```

| Field | Required | Meaning |
|---|---|---|
| `type` | yes | Exactly `openrouter`. |
| `api_key` | yes | Inference key: 1-256 printable ASCII characters, no spaces. The `sk-or-` prefix is not required (the shape is not a documented contract). |
| `management_api_key` | no | Used **only** for `GET /api/v1/credits` (account balance). Use a read-only management key. Must differ from `api_key`. Without it the account-balance bucket is absent. |
| `min_remaining_usd` | no | Number from 0 to 1000000, default 0. The guard refuses when the binding limit is at or below it. |
| `allow_unlimited_key` | no | Boolean, default `false`. Allows a key without a credit limit. |
| `label`, `prefix`, `proxy_url`, `weight`, `priority`, `disabled` | no | Host-owned, validated by the kit. |

Validation is strict, and every failure is `400 invalid_auth` with a fixed
message that never contains a key. An `id` field is rejected (the auth ID is
`openrouter:` plus 12 hex characters of the key's SHA-256). Unknown fields are
rejected, so a typo such as `min_remaining` cannot silently drop the reserve;
the host-owned fields CLIProxyAPI v8.0.49 itself writes into auth files (`note`,
`headers`, `request_retry`, `disable_cooling`, `excluded_models`,
`excluded_models_hash`, `websockets`) are accepted and ignored. The same key in
two files is rejected as `duplicate_auth` (only SHA-256 digests are kept).

The plugin YAML has no settings. No CLIProxy configuration is required.

## Models and execution

Model discovery calls `GET /api/v1/models` per auth through the host HTTP client
(no probes: probes cost money). Every registered ID is `openrouter/` plus
OpenRouter's ID. The prefix is stripped exactly once before dispatch:

| Client model ID | Sent to OpenRouter |
|---|---|
| `openrouter/anthropic/claude-sonnet-4.5` | `anthropic/claude-sonnet-4.5` |
| `openrouter/meta-llama/llama-3.3-70b-instruct:free` | `meta-llama/llama-3.3-70b-instruct:free` |
| `openrouter/openrouter/free` | `openrouter/free` |
| `openrouter/~anthropic/claude-sonnet-latest` | `~anthropic/claude-sonnet-latest` |

With an auth `prefix` such as `acct`, the host also publishes
`acct/openrouter/...`; no bare `vendor/model` ID is ever published.

An entry is not registered when:

1. its ID is empty, longer than 128 characters, or not `vendor/model` (one `/`
   after an optional `~`; `:variant` suffixes allowed);
2. it ends in `:batch` (Batch API only);
3. `architecture.output_modalities` exists and lacks `text`;
4. `pricing.prompt` or `pricing.completion` is negative (`"-1"`: variable-price routers);
5. `expiration_date` is today or earlier (UTC);
6. it duplicates an earlier ID, or does not decode.

Routing suffixes that are not catalog entries (`:nitro`, `:floor`, `:exacto`,
`:online`) are not registered. Static registration is empty; the catalog comes
only from discovery. The pinned host re-runs discovery when an auth is
(re)registered and when the plugin is reloaded, not on a timer; the plugin caches
each auth's catalog for 10 minutes. New OpenRouter models therefore appear after
an auth or plugin reload.

Execution is OpenAI chat only (`POST /api/v1/chat/completions`). Messages and
Responses clients go through the host's translation from chat. The body passes
through unchanged except `model` and `stream`, including `provider`, `models`,
`reasoning`, `plugins` and `session_id`. Headers are exactly `Authorization`,
`Content-Type` and `Accept`; raw HTTP requests cannot change the URL, method or
headers. Streams are relayed unbuffered, including OpenRouter's
`: OPENROUTER PROCESSING` comments and the final usage chunk.

## Quota

The quota card is display-only: the host never routes on it. It reads
`GET /api/v1/key` (and `GET /api/v1/credits` with a management key) through the
plugin's own bounded client (GET only, 15 s, 1 MiB, no redirects, auth or host
proxy, never direct on a bad proxy). Readings are cached 60 s per auth and shared
with the guard; errors are never cached.

### Buckets

| Group | Bucket | Remaining fraction | Reset |
|---|---|---|---|
| Key credit limit | `<daily\|weekly\|monthly\|lifetime\|unknown-reset> key limit` | `limit_remaining / limit`, clamped to 0-1 | `budgets[scope=key_limit].resets_at` when plausible, else the UTC calendar rule; none for lifetime/unknown |
| Key credit limit | `Key limit: NOT SET (plugin refuses requests)` | 0 | none |
| Key credit limit | `Key limit: NOT SET (UNBOUNDED spend)` (with `allow_unlimited_key`) | 1 | none |
| Budgets | `<interval> <member guardrail\|key guardrail\|workspace\|unknown> budget`, one per `budgets[]` entry except `key_limit` (max 16) | `remaining_usd / limit_usd` (or `limit_usd - spend_usd`) | `resets_at`, else the calendar rule |
| Free models (:free) | `daily free-model requests` (when the daily limit is above 0) | `remaining / limit` | next 00:00 UTC |
| Account credits | `account credit balance (no reset; top-up)` (with a management key) | `(total_credits - total_usage) / total_credits` | none |

A zero key limit shows fraction 0 (`key limit is $0`). A negative
`limit_remaining` shows as a negative amount with fraction 0. The budget that
matches `effective_budget` is marked `binding`.

### Summary metrics

The spend windows are counters, not caps, so they are metrics in USD rather than
buckets (a bucket needs a remaining fraction, which an uncapped window lacks).
The cap that governs a window is a bucket above, with its reset.

| Key | Source |
|---|---|
| `key_limit_usd`, `key_remaining_usd` | `limit`, `limit_remaining` |
| `key_spend_day_usd`, `key_spend_week_usd`, `key_spend_month_usd`, `key_spend_total_usd` | `usage_daily`, `usage_weekly`, `usage_monthly`, `usage` |
| `byok_spend_{day,week,month,total}_usd` | `byok_usage*`, only when `byok_usage` is above 0 |
| `account_balance_usd` | `total_credits - total_usage` |
| `spend_reserve_usd` | `min_remaining_usd`, when above 0 |
| `free_requests_used` | `free_model_daily_requests.used` |
| `key_expires_in_days` | `expires_at - now`, 1 decimal |

Money metrics use format `currency` and currency `USD`. A metric is emitted only
when its source is a finite number; fields of the wrong JSON type read as absent.
The subscription tier is `NO KEY LIMIT`, `free tier` or `paid credits`. A
management key used as `api_key` is reported as `invalid_credentials`.

### Reset rules

All windows are UTC calendar windows (OpenRouter limits documentation): daily at
00:00, weekly on Monday 00:00, monthly on the first at 00:00. A server
`resets_at` is used when it is in the future and at most 32 days away; otherwise
the calendar rule applies. Lifetime and unknown windows have no reset; their
exhaustion is re-checked hourly.

### Spend guard

Before every Execute, ExecuteStream and HttpRequest the guard takes the auth's
key reading (re-read when older than 60 s; single-flight per auth) and applies,
in order:

| Rule | Condition | Result |
|---|---|---|
| G0 | No reading, a failed re-read with no good reading from the last 10 minutes, or the host proxy not yet known | `429 quota_unverified`, retry 60 s |
| G1 | The key is a management key | `401 invalid_credentials` |
| G2 | No key limit and `allow_unlimited_key` is false | `403 key_limit_required` |
| G3 | The lowest of key remaining, effective-budget remaining and account balance is at or below `min_remaining_usd` | `429 quota_exhausted`; the retry is the longest wait among the blocking layers: until its reset, or 3600 s for a layer that never resets (lifetime, unknown, account balance) |
| G4 | A `:free` model while the daily free-model requests are used up | `429 quota_exhausted` until 00:00 UTC |

It fails closed: an unreadable key status stops inference on that auth. After
any upstream limit, budget or credit error the auth's reading is invalidated,
so the next request re-reads it.

## Failure classification

Upstream text and keys are never copied into errors. Messages are fixed strings
prefixed with the code.

| Upstream | Condition | Status | Code | Retry after |
|---|---|---|---|---|
| 402 or 403 | `limit_source: openrouter_key_limit` | 429 | `quota_exhausted` | until `resets_at` or the key's window reset; 3600 for lifetime/unknown |
| 402 or 403 | `openrouter_guardrail_budget` or `openrouter_workspace_budget` | 429 | `quota_exhausted` | until `resets_at`; 3600 when null (lifetime) |
| 402 | `openrouter_in_flight_budget` or `in_flight_budget_exhausted` | 429 | `rate_limited` | `Retry-After`, else 5 (the host floors at 10) |
| 402 | `openrouter_credits` with `weight_exceeds_budget` | 400 | `request_exceeds_budget` | none (request-scoped) |
| 402 | any other | 429 | `quota_exhausted` | 3600 (top-up) |
| 403 | another `limit_source` | 429 | `quota_exhausted` | `resets_at`, else 3600 |
| 401 | any | 401 | `invalid_credentials` | none |
| 403 | `error_type` content_policy_violation, moderation or refusal, or a moderation message | 400 | `content_policy` | none (request-scoped) |
| 403 | other | 403 | `entitlement_required` | none |
| 404 | any | 404 | `upstream_not_found` (message carries `model_not_supported`) | none (host: 12 h) |
| 400, 413, 422 | any | same | `invalid_request` | none |
| 408 | any | 408 | `timeout` | `Retry-After` |
| 429 | `:free` model and no free requests left today | 429 | `quota_exhausted` | until 00:00 UTC |
| 429 | other | 429 | `rate_limited` | `Retry-After`, else `X-RateLimit-Reset`, else host backoff |
| 500, 502, 503, 504, 524, 529 | JSON body | same | `upstream_unavailable` | `Retry-After` |
| 5xx or other | non-JSON body, or another status | same | `upstream_error` | `Retry-After` |

Only the first 64 KiB of an error body is parsed. A `Retry-After` never shortens
a known exhaustion; a longer one wins. `X-RateLimit-Reset` units are not
documented: values from 1e12 are read as epoch milliseconds, from 1e9 as epoch
seconds, else as seconds to wait. A non-streaming `200` whose body is only an
`error` object (no choices) is classified as if its `error.code` were the status.
In the pinned host, `request_exceeds_budget` and `content_policy` cause no
cooldown (verified by the host-compat test).

## Pinned host RPC limitation

CLIProxyAPI v8.0.49 keeps only the status, message and retry hint of a plugin
error: the plugin's error code is dropped, and there is no credential-scope or
transient marker. 401, 402 and 403 always cool the model for a fixed 30 minutes
and ignore any retry hint; only a 429 with a retry hint honors a real reset time.
So a spent key limit, budget or balance is reported as **429 `quota_exhausted`
with a retry hint until the reset**, as the Cloudflare Workers AI plugin does.
Clients see 429 instead of OpenRouter's 402/403.

Every plugin failure also cools only one (auth, model) pair. The guard makes up
for that: once the key is spent, each other model of that auth is refused
locally once, with no upstream call, and then cools until the same reset.

The upstream fix that would remove this: preserve the plugin error code in the
host's `resultErrorFromError` and expose credential-scope and transient markers
on the RPC error type.

## Mid-stream errors

When OpenRouter fails after a stream has started (HTTP 200 followed by a chunk
with `finish_reason: "error"`), the chunk is relayed raw and not classified, as in
the other provider plugins.

## Build, test, package

```bash
go test -race -count=1 ./...
go -C integration/hostcompat test -race -count=1 ./...
bash scripts/package-release.sh dist
```

The release script builds `openrouter.so`, checks the `cliproxy_plugin_init`
entry point, refuses an artifact importing GLIBC above 2.34, and writes
`openrouter_<version>_linux_amd64.zip` and `checksums.txt`. The version string
lives in `main.go`, `registry.json`, `packaging_test.go`, the host-compat test
and this README; bump all of them together. Never install a library built with
the `hostcompat` tag: it only accepts a loopback test upstream.

## Pending live verification

These are documented by OpenRouter but not yet observed live. Settle them on a
staging instance with a key that has a $0.05 monthly limit (see
[docs/MIGRATION.md](docs/MIGRATION.md)):

1. Whether a spent key limit returns 402 or 403 (both are handled by `limit_source`).
2. Whether `budgets[]` carries a `key_limit` entry with `resets_at` (else the
   calendar rule is used).
3. Whether `limit_remaining` can go negative (clamped either way).
4. The unit of `X-RateLimit-Reset`.
5. Whether `budgets` and `effective_budget` appear for personal keys.
6. The exact `limit_reset` strings (an unknown one shows as `unknown-reset` and
   is re-checked hourly).

Also unverified: the `endpoints[]` contract, the key shape, whether `:free`
requests are blocked by a spent key limit (the guard refuses them anyway), and
whether the in-flight budget applies to a given account.

## Sources

See [docs/SOURCES.md](docs/SOURCES.md) for the wire contract, the OpenRouter
documentation used (with fetch dates) and the host source lines this plugin
depends on.

No real provider inference, quota or model endpoint is called by this test suite.
