# Staging runbook

This is an operator runbook, not an automated deployment. The build, test and
packaging commands never touch a running CLIProxy. Production installs need the
owner's explicit confirmation after staging acceptance.

## 1. Prepare

1. Privately back up the staging CLIProxy config, auth directory and plugin
   directory. Never commit the backup or any key.
2. Obtain the reviewed release and verify the archive against `checksums.txt`.
   Never install a library built with the `hostcompat` tag.
3. Use a regular OpenRouter API key for staging with a **credit limit** and a
   reset window (for example weekly) set at
   [openrouter.ai/settings/keys](https://openrouter.ai/settings/keys). No
   management key is used. The owner chose not to create a tiny test key to
   force exhaustion, so the exhaustion path stays covered by tests only.
4. Confirm no `openai-compatibility` entry named `openrouter` exists in the
   staging config: a native entry with the same provider name would share the
   host's OpenRouter special-casing and compete for the provider.

## 2. Install on staging only

Install through the native plugin store using this repository's registry, or
place the verified binary in the staging plugin directory under `linux/amd64`.
All locations below are placeholders.

```yaml
plugins:
  enabled: true
  dir: <staging-plugin-directory>
  store-sources:
    - https://raw.githubusercontent.com/TogetherWeOwn/cliproxy-provider-openrouter/main/registry.json
  configs:
    openrouter:
      enabled: true
```

Create one auth file per key (mode `0600`) with the README schema. Do not set an
`id`. Do not print keys into shell history, logs, issues or public artifacts.

## 3. Staging acceptance

1. Plugin registered with version 0.1.0 and quota support; the auth parses and
   persists; a second file with the same key is rejected.
2. The model list contains only `openrouter/...` IDs and no `-1` router.
3. The quota card shows the key limit with its reset (from
   `budgets[scope=key_limit].resets_at`), the free-model requests, the spend
   metrics and the account balance, all read with the regular key.
4. One small request to a cheap model succeeds, streaming and non-streaming.
   With a reasoning model and a tiny `max_tokens`, `null` content with
   `finish_reason: "length"` is a success.
5. Do not spend a key to its limit on purpose. If a key limit is ever reached
   in normal use, record privately whether OpenRouter answered 402 or 403: the
   client must see 429 `quota_exhausted` with a retry hint until the reset, and
   other models on the auth must be refused without upstream calls even while
   `GET /key` still shows headroom.
6. Record any `X-RateLimit-Reset` value seen on a 429.

Update the README's live-verification section and `docs/SOURCES.md` with any
new result before any production request.

## 4. Rollback

Disable the plugin config entry (or remove the plugin binary) and the auth
files, then reload CLIProxy. The plugin keeps no persistent state.
