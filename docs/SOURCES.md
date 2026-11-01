# Sources and wire contract

All OpenRouter documents were fetched on 2026-11-01. Where a live response
(2026-11-01, redacted in `testdata/live_*.json`) differs from the documents,
the live response wins and is noted below.

## OpenRouter documentation

| Topic | Source |
|---|---|
| `GET /api/v1/key`, `GET /api/v1/credits`, `GET /api/v1/models` schemas | https://openrouter.ai/openapi.json |
| Credit limits, budgets, reset windows, free-model limits, in-flight budget, 402/403/429 handling | https://openrouter.ai/docs/api_reference/limits.md |
| Error envelope, `error_type` values, moderation 403, mid-stream errors, `Retry-After` | https://openrouter.ai/docs/api_reference/errors-and-debugging.md |
| Documentation index | https://openrouter.ai/docs/llms.txt |

## Wire contract used by the plugin

`GET /api/v1/key` (regular key) returns `data` with: `limit`,
`limit_remaining`, `limit_reset` (`daily`, `weekly`, `monthly` or null),
`include_byok_in_limit`, `usage`, `usage_daily`, `usage_weekly`,
`usage_monthly`, the matching `byok_usage*` fields, `is_free_tier`,
`is_management_key` (and the deprecated `is_provisioning_key`), `expires_at`,
`free_model_daily_requests {used, limit, remaining}`, `budgets[]` and
`effective_budget`. A budget has `scope` (`workspace_default_member`,
`workspace_default_key`, `member`, `api_key`, `workspace`, `key_limit`),
`guardrail_id`, `budget_id`, `limit_usd`, `spend_usd`, `remaining_usd`,
`reset_interval` (`daily`, `weekly`, `monthly` or null for lifetime),
`resets_at` (ISO 8601 UTC or null) and `include_byok`. `label`, `rate_limit`
(deprecated), creator IDs and regions are ignored and never stored.

`GET /api/v1/credits` returns `data {total_credits, total_usage}`. The
documents say it needs a management key; live, the regular key gets 200, so
the plugin reads it with the regular key and omits only the balance on any
failure. No management key is used.

Live (2026-11-01): `budgets[]` holds a `scope: key_limit` entry whose
`resets_at` is the key's next reset, and `effective_budget` equals it;
`limit_reset` is `"weekly"`; the `/key` usage counters lag real spend; a
`:free` 503 carries `message: "Provider returned error"` and
`metadata.raw: "no upstreams available"` (an outage, not quota).

`GET /api/v1/models` without query parameters returns the full text catalog:
`id`, `name`, `created`, `description`, `context_length`,
`architecture {input_modalities, output_modalities}`,
`pricing {prompt, completion}` (strings; `"-1"` for variable-price routers),
`top_provider.max_completion_tokens`, `supported_parameters`,
`reasoning {supported_efforts, mandatory}` and `expiration_date`.

Errors use `{"error": {"code", "message", "metadata"}}`. Limit errors carry
`metadata.limit_source` (`openrouter_key_limit`, `openrouter_guardrail_budget`,
`openrouter_workspace_budget`, `openrouter_credits`,
`openrouter_in_flight_budget`), and budget errors `scope` and `resets_at`.
The in-flight 402 carries `reason: in_flight_budget_exhausted` and a
`Retry-After` header; a request larger than the whole in-flight budget carries
`reason: weight_exceeds_budget`. Platform 429s carry `X-RateLimit-Limit`,
`X-RateLimit-Remaining` and `X-RateLimit-Reset`.

Spend windows are UTC calendar windows: daily at 00:00, weekly on Monday 00:00,
monthly on the first at 00:00.

## Unverified (docs only)

1. A spent key limit is documented as both 402 and 403; the plugin keys on
   `limit_source`.
2. Whether `limit_remaining` can go negative.
3. The unit of `X-RateLimit-Reset`.
4. The `endpoints[]` contract, the key shape, whether `:free` requests are
   blocked by a spent key limit, and which accounts the in-flight budget covers.

## Host facts (CLIProxyAPI v8.0.49)

| Fact | Source |
|---|---|
| A plugin RPC error keeps only code, message, status and retry hint; the code does not reach cooldown, and there is no credential-scope or transient marker | `internal/pluginhost/rpc_client.go` (`rpcError`); `sdk/cliproxy/auth/conductor_cooldown.go` (`resultErrorFromError`) |
| 401/402/403 cool a model for a fixed 30 minutes; 404 uses the hint or 12 hours; 429 uses max(hint, 10 s) or exponential backoff | `sdk/cliproxy/auth/conductor_cooldown.go` (the status switch in the model-state update) |
| A 400/404/422 whose message contains `model_not_supported` is a model-support failure | `sdk/cliproxy/auth/conductor_cooldown.go` (`isModelSupportErrorMessage`) |
| Quota cards are display-only | `internal/api/handlers/management/plugin_quota.go` |
| The auth `prefix` adds `prefix/<id>` next to each plugin ID; plugin IDs are registered as returned. A model `Name` is rewritten with the prefix only when it equals the ID, so the plugin sets `Name` to the namespaced ID | `sdk/cliproxy/service_models.go` (`applyModelPrefixes`, `rewriteModelInfoName`) |
| The Gemini model listing emits `Name` (else `ID`) as the model name | `internal/registry/model_registry.go` (the `gemini` case of the model-info conversion) |
| A 503 cools the model briefly as a transient error | `sdk/cliproxy/auth/conductor_cooldown.go` (the 408/5xx transient branch) |
| Discovery runs on auth (re)registration and plugin/config reload; the periodic model refresh callback is driven by the built-in remote catalog, not by plugins | `sdk/cliproxy/service_plugins.go` (`refreshPluginModelRegistrations`, `registerModelRefreshCallback`) |
| The provider name `openrouter` enables OpenRouter-specific reasoning handling and subset token accounting | `internal/thinking/summary.go`; `sdk/cliproxy/usage/accounting.go` |
| An empty static model list registers no static models and leaves the executor and per-auth discovery in place | `internal/pluginhost/adapters.go` (`RegisterModels`); asserted by the host-compat test |
| The management API can write host fields (`note`, `headers`, `request_retry`, `excluded_models`, ...) into an auth file | `internal/api/handlers/management/auth_files_fields.go` |
