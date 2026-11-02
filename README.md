# cliproxy-provider-openrouter

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) provider plugin for
[OpenRouter](https://openrouter.ai). It is built on
[cliproxy-provider-kit](https://github.com/TogetherWeOwn/cliproxy-provider-kit)
(pinned at `88850ce`), targets CLIProxyAPI **v8.0.49** (plugin RPC schema 10),
and is tested against the real v8.0.49 plugin loader. It adds no scheduler, no
background poller and no host configuration.

What it does:

- reads plugin-owned auth files (`"type": "openrouter"`);
- registers OpenRouter's catalog under the fixed `openrouter/` namespace, so it
  never registers a bare `vendor/model` ID that could clash with an official
  provider;
- executes OpenAI chat completions on OpenRouter through the host HTTP client;
- refuses inference before dispatch when the key's credit limit (or any budget
  over it) is spent, has no limit, or cannot be read;
- shows the key limit, budgets, free-model requests and the account balance as a
  quota card, with the right reset time for each window.

It needs only the **regular OpenRouter API key**. No management key is used or
accepted.

The response shapes were checked live on 2026-11-01 against a real key; the
redacted responses are the `testdata/live_*.json` fixtures and the tests assert
the exact quota card built from them. See [Live verification](#live-verification)
for what is still unobserved. Run the staging acceptance in
[docs/MIGRATION.md](docs/MIGRATION.md) before any production use.

## Warning: this spends real money

- OpenRouter bills real USD per request.
- **Set a key limit.** Always set a credit limit and a reset window (for
  example weekly) on the key at
  [openrouter.ai/settings/keys](https://openrouter.ai/settings/keys). OpenRouter
  enforces that limit server side; it is the only hard stop.
- A key **without** a credit limit is refused (`403 key_limit_required`) unless
  its auth file sets `"allow_unlimited_key": true`. Even then its quota card
  never shows headroom for the key (fraction 0).
- `min_remaining_usd` is a soft reserve. OpenRouter's `GET /key` usage counters
  **lag real spend** (live: two paid requests still read `usage: 0` seconds
  later), and the plugin keeps no local spend tally. So the reserve can be
  overshot by everything spent before the counters catch up. The key limit still
  caps the total.
- Variable-price routers (pricing `"-1"`, for example `openrouter/auto`) are not
  registered, because their cost is unknowable before the call.

## Auth files

One JSON file per key, mode `0600`, in the CLIProxy auth directory. Never commit
one.

```json
{
  "type": "openrouter",
  "api_key": "<regular OpenRouter API key>",
  "min_remaining_usd": 0.5,
  "allow_unlimited_key": false,
  "label": "OpenRouter main"
}
```

| Field | Required | Meaning |
|---|---|---|
| `type` | yes | Exactly `openrouter`. |
| `api_key` | yes | The regular OpenRouter API key, used for inference, `GET /api/v1/key` and `GET /api/v1/credits`: 1-256 printable ASCII characters, no spaces. The `sk-or-` prefix is not required (the shape is not a documented contract). A management key here is refused (`401 invalid_credentials`). |
| `min_remaining_usd` | no | Number from 0 to 1000000, default 0. The guard refuses when the binding limit is at or below it. |
| `allow_unlimited_key` | no | Boolean, default `false`. Allows a key without a credit limit. |
| `label`, `prefix`, `proxy_url`, `weight`, `priority`, `disabled` | no | Host-owned, validated by the kit. |

Validation is strict, and every failure is `400 invalid_auth` with a fixed
message that never contains a key. A `management_api_key` field is rejected
with a message naming it: the plugin uses only the regular key. An `id` field
is rejected (the auth ID is
`openrouter:` plus 12 hex characters of the key's SHA-256). Unknown fields are
rejected, so a typo such as `min_remaining` cannot silently drop the reserve;
the host-owned fields CLIProxyAPI v8.0.49 itself writes into auth files (`note`,
`headers`, `request_retry`, `disable_cooling`, `excluded_models`,
`excluded_models_hash`, `websockets`) are accepted and ignored. The same key in
two files is rejected as `duplicate_auth` (only SHA-256 digests are kept). When
the file holding a key is moved or renamed, the key passes to the file now
holding it once the old path no longer exists.

The plugin YAML has no settings. No CLIProxy configuration is required.

## Management-UI login

The management UI's **Start OpenRouter Login** card runs a real PKCE OAuth
login, copying the official CLIProxy providers: the card opens the OpenRouter
approval URL itself (`openrouter.ai/auth`, with the S256 challenge and the
host loopback callback URL tagged with the login state) in the panel's
standard OAuth dialog. The operator approves, then copies the loopback
callback URL from their address bar into the panel's callback box; the host's
own OAuth callback extracts the one-time code into a `0600`
`.oauth-openrouter-<state>.oauth` file (published atomically), and the next
poll exchanges it for an API key (`POST /api/v1/auth/keys`, code + verifier,
S256) — exactly one poller runs the exchange — deletes the file, and runs the
same ParseAuth validation and duplicate-key guard as an uploaded auth file, so
logging in twice with the same key is idempotent. A denied approval closes the
login with an error. Expired sessions, evicted sessions, and dead polls take
their code files with them, and a new login sweeps leftovers older than the
session lifetime. On a remote host, serve the management UI over `https://`
so the pasted callback URL is encrypted in transit.

## Models and execution

Model discovery calls `GET /api/v1/models` per auth through the host HTTP client
(no probes: probes cost money). Fixed model namespace `openrouter/`; auth
`prefix` does not replace it. Every model identifier the
plugin publishes, both `id` and `name` (the host lists `name` as the Gemini
model name), carries `openrouter/` exactly once, with no exception for `~`
aliases, `:free` variants or OpenRouter's own `openrouter/...` routers (those
already carry it, so they pass through instead of doubling). Dispatch inverts
the mapping:

| Client model ID | Sent to OpenRouter |
|---|---|
| `openrouter/anthropic/claude-sonnet-4.5` | `anthropic/claude-sonnet-4.5` |
| `openrouter/meta-llama/llama-3.3-70b-instruct:free` | `meta-llama/llama-3.3-70b-instruct:free` |
| `openrouter/free` | `openrouter/free` |
| `openrouter/~anthropic/claude-sonnet-latest` | `~anthropic/claude-sonnet-latest` |

With an auth `prefix` such as `acct`, the host also publishes
`acct/openrouter/...` (its name is rewritten with it); no bare `vendor/model` ID
or name is ever published.

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
Responses clients go through the host's translation from chat (OpenRouter's
native `POST /api/v1/messages` works live, but this plugin does not use it). The body passes
through unchanged except `model` and `stream`, including `provider`, `models`,
`reasoning`, `plugins` and `session_id`. Headers are exactly `Authorization`,
`Content-Type` and `Accept`; raw HTTP requests cannot change the URL, method or
headers. Streams are relayed unbuffered, one SSE event per chunk, including the
final usage chunk; comments are dropped (`: OPENROUTER PROCESSING`), and so is
`[DONE]`, which the host appends itself. OpenAI chat clients get bare JSON
events (the host skips translation for this same-format stream and its handler
adds `data: `); every other client format gets `data:`-framed events for the
host translator. The plugin tells them apart by the inbound route the host
records (`request_path`), because the host reports the plugin's own `openai`
format as the source format for every client. A `200` whose
content is `null` with `finish_reason: "length"` is a success: reasoning used up
`max_tokens` (seen live); raise `max_tokens`.

## Quota

The quota card is display-only: the host never routes on it. It reads
`GET /api/v1/key` and, concurrently, `GET /api/v1/credits`, both with the
regular key, through the plugin's own bounded client (GET only, 15 s for `/key`
and 5 s for `/credits`, 1 MiB, no redirects, auth or host proxy, never direct
on a bad proxy). OpenRouter documents `/credits` as management-key only, but it
answers the regular key (seen live). If `/credits` fails for any reason (401,
403, 5xx, timeout, bad body), only the account-balance bucket and its guard
layer are omitted; everything else still works. Readings are cached 60 s per
auth, never past the first reset after they were taken (the next 00:00 UTC, or
an earlier `resets_at` of the key limit or effective budget), and shared with
the guard; errors are never cached.

### Buckets

One `OpenRouter` group holds short-labelled bars with no descriptions:

| Bucket | Remaining fraction | Reset |
|---|---|---|
| `daily` / `weekly` / `monthly` / `lifetime` key-limit id (or `Key limit` when the reset window is unknown) | `limit_remaining / limit`, clamped to 0-1 | `budgets[scope=key_limit].resets_at` when plausible, else the UTC calendar rule; none for lifetime/unknown |
| `Key limit unreadable` (limit present but not a number; plugin refuses requests) | 0 | none |
| `<Daily\|Weekly\|Monthly\|Lifetime> <member guardrail\|key guardrail\|workspace\|unknown> budget`, one per `budgets[]` entry except `key_limit` | `remaining_usd / limit_usd` (or `limit_usd - spend_usd`); uncomputable stays a 0% warning bar | `resets_at`, else the calendar rule |
| `Free models` (when the daily free-model limit is above 0) | `remaining / limit` | next 00:00 UTC |

A key without a limit shows no key-limit bucket at all: a 0-fraction bucket
reads as exhausted, so the no-limit hunk omits the bucket and the card keeps
the `NO KEY LIMIT` tier. A zero key limit, a negative `limit_remaining`, and an
unknown remaining all show a 0% warning bar. Whether requests are admitted is
the spend guard's job (`allow_unlimited_key`), not the card's.

Usage counters (key spend per window, BYOK splits, the spend reserve, the
account-credits bar, lifetime purchased) stay off the card because they repeat
the bars or are not usage limits.

### Summary metrics

At most three chips, none repeating a bar:

| Key | Source |
|---|---|
| `account_balance_usd` (`Balance`) | `total_credits - total_usage`, format `currency` USD |
| `key_expires_in_days` (`Key expires in`) | whole days from `expires_at - now`, minimum 1 while unexpired and 0 once elapsed, so an expired cached reading keeps its indicator instead of reading as no expiry |

Lifetime spend stays off the card: it repeats the lifetime bar's subject,
and the full history remains in the OpenRouter dashboard.

A metric is emitted only when its source is a finite number; fields of the
wrong JSON type read as absent. The subscription tier is `KEY LIMIT UNREADABLE`,
`NO KEY LIMIT`, `free tier` or `paid credits`. A
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
| G0 | No reading, a failed re-read with no good reading from the last 10 minutes (and none from before a reset), every re-read overtaken by a newer limit error, a `limit` that is present but not a number, an effective budget with a limit but no remaining amount, or the host proxy not yet known | `429 quota_unverified`, retry 60 s |
| G1 | The key is a management key | `401 invalid_credentials` |
| G2 | No key limit and `allow_unlimited_key` is false | `403 key_limit_required` |
| G3 | The lowest of key remaining, effective-budget remaining and account balance is at or below `min_remaining_usd` | `429 quota_exhausted`; the retry is the longest wait among the blocking layers: until its reset, or 3600 s for a layer that never resets (lifetime, unknown, account balance) |
| G4 | A `:free` model while the daily free-model requests are used up | `429 quota_exhausted` until 00:00 UTC |
| G5 | An exhaustion latch is set for the auth (all models), or for its `:free` models | `429 quota_exhausted` until the latch ends |

It fails closed: an unreadable key status stops inference on that auth. After
any upstream limit, budget or credit error the auth's reading is invalidated,
so the next request re-reads it.

Because the `/key` counters lag, a re-read right after OpenRouter reported a
spent limit can still show money left. So every upstream exhaustion also sets a
per-auth **latch** until its retry time: a key limit, budget, credit or other
spend-limit error latches every model of the auth; the daily free-model quota
latches only its `:free` models. A latch ends early only when a later reading
shows the owner changed the cap that failed: the key limit or its reset window
for a key-limit latch, the effective budget's limit for a budget latch, a
top-up in `total_credits` for a credit latch, a higher daily free-model limit
for a free latch (any of the first three for another spend limit). A top-up
does not lift a spent key limit, and a reading that merely still shows money
or requests left does not end any latch. Latches live in memory and start over on a key change or a
plugin restart.

## Failure classification

Upstream text and keys are never copied into errors. Messages are fixed strings
prefixed with the code.

| Upstream | Condition | Status | Code | Retry after |
|---|---|---|---|---|
| 402 or 403 | `limit_source: openrouter_key_limit` | 429 | `quota_exhausted` | until `resets_at` or the key's window reset; 3600 for lifetime/unknown |
| 402 or 403 | `openrouter_guardrail_budget` or `openrouter_workspace_budget` | 429 | `quota_exhausted` | until `resets_at`; 3600 when null (lifetime) |
| 402 or 403 | `openrouter_in_flight_budget` or `in_flight_budget_exhausted` | 429 | `rate_limited` | `Retry-After`, else 5 (the host floors at 10) |
| 402 | `openrouter_credits` with `weight_exceeds_budget` | 400 | `request_exceeds_budget` | none (request-scoped) |
| 402 | any other | 429 | `quota_exhausted` | 3600 (top-up) |
| 403 | another `limit_source` | 429 | `quota_exhausted` | `resets_at`, else 3600 |
| 401 | any | 401 | `invalid_credentials` | none |
| 403 | `error_type` content_policy_violation, moderation or refusal, or a moderation message | 400 | `content_policy` | none (request-scoped) |
| 403 | other | 403 | `entitlement_required` | none |
| 404 | any | 404 | `upstream_not_found` (message carries `model_not_supported`) | none (host: 12 h) |
| 400, 413, 422 | any | same | `invalid_request` | none |
| 408 | any | 408 | `timeout` | `Retry-After` |
| 429 | message names `free-models-per-day`, or a `:free` model with no free requests left in the last reading | 429 | `quota_exhausted` (latches the auth's `:free` models) | until 00:00 UTC |
| 429 | other | 429 | `rate_limited` | `Retry-After`, else `X-RateLimit-Reset`, else host backoff |
| 500, 502, 503, 504, 524, 529 | JSON body, including the live `:free` 503 `Provider returned error` / `no upstreams available` | same | `upstream_unavailable` (provider outage: short host cooldown, never quota, no re-read) | `Retry-After` |
| 5xx or other | non-JSON body, or another status | same | `upstream_error` | `Retry-After` |

Only the first 64 KiB of an error body is parsed. A `Retry-After` never shortens
a known exhaustion; a longer one wins. `X-RateLimit-Reset` units are not
documented: values from 1e12 are read as epoch milliseconds, from 1e9 as epoch
seconds, else as seconds to wait. A non-streaming `200` whose body is only an
`error` object (no choices) is classified as if its `error.code` were the status,
on both the Execute and the raw HTTP path.
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

Every plugin failure also cools only one (auth, model) pair. The guard's latch
makes up for that: once OpenRouter reports the key spent, each other model of
that auth is refused locally once, with no upstream call, and then cools until
the same reset, even while the lagging `/key` counters still show headroom.

The upstream fix that would remove this: preserve the plugin error code in the
host's `resultErrorFromError` and expose credential-scope and transient markers
on the RPC error type.

## Mid-stream errors

When OpenRouter fails after a stream has started (HTTP 200 followed by a chunk
with `finish_reason: "error"`), the event is relayed unchanged and not
classified, as in the other provider plugins.

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

## Live verification

Checked live on 2026-11-01 with a real key (redacted responses:
`testdata/live_key_inference.json`, `testdata/live_credits_inference.json`,
`testdata/live_key_management.json`):

1. `GET /key` with the regular key returns every documented field:
   `limit: 5`, `limit_reset: "weekly"`, `limit_remaining: 5`.
2. `budgets[]` has a `scope: key_limit` entry with
   `resets_at: 2026-11-02T00:00:00.000Z` (next Monday 00:00 UTC), and
   `effective_budget` is that entry; budgets appear for this key.
3. `free_model_daily_requests` is `{used: 0, limit: 1000, remaining: 1000}`;
   the deprecated `rate_limit` reports `-1`/`"10s"` and is ignored.
4. `GET /credits` answers the regular key (`total_credits: 245`,
   `total_usage: 237.537304708`), contrary to the documentation.
5. A management key reads `is_management_key: true` with no limit or budgets;
   the guard refuses it as an inference key.
6. Usage counters lag: after two paid requests `usage` still read 0 seconds
   later. The guard never treats `/key` as real time (see the latch above).
7. A `:free` model returned 503 `Provider returned error` with
   `metadata.raw: "no upstreams available"`: a provider outage, not quota.
8. Empty content with `finish_reason: "length"` means reasoning used up
   `max_tokens`. `POST /api/v1/messages` works natively. No `X-RateLimit-*`
   headers were sent on success.

Still unobserved: whether a spent key limit answers 402 or 403 (both are
handled by `limit_source`), whether `limit_remaining` can go negative (clamped
either way), the unit of `X-RateLimit-Reset`, the `endpoints[]` contract,
whether `:free` requests are blocked by a spent key limit (the guard refuses
them anyway), and which accounts the in-flight budget covers.

## Sources

See [docs/SOURCES.md](docs/SOURCES.md) for the wire contract, the OpenRouter
documentation used (with fetch dates) and the host source lines this plugin
depends on.

No real provider inference, quota or model endpoint is called by this test suite.
