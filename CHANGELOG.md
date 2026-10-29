# Changelog

## Unreleased

### Added

- **Caller priority.** `client_priorities: {name: low|normal|high}` ranks
  callers (default `normal`), and an upstream's `min_client_priority`
  (default `low`, everyone) keeps lower callers off it, ahead of any
  strategy, failover or fallback, on `/v1/messages`, `/v1/responses` and
  `/v1/messages/count_tokens`. A request that only restricted upstreams can
  serve gets `403` with `X-Tokenpool-Refused: caller-priority`; a limit
  error names the upstreams the caller's priority excluded. The admin API
  shows and sets the field (`POST /admin/upstreams/{name}/min_client_priority`,
  or POST/PUT of an API upstream), `/admin/status` lists callers, logs carry
  `priority=`, and `-check` prints a `MIN CALLER` column. Existing configs
  need no change.

## v0.2.1 (2026-10-25)

### Fixed

- A 429 no longer benches a whole upstream until a model-specific limit
  resets. Claude's weekly Opus limit (`7d_opus`) at 100% used to hold the
  account for up to `cooldowns.max` (24h by default), so Sonnet requests
  that would have worked were locked out too. Now the upstream waits for
  its exhausted account-wide window (the 5-hour one, say) or its soonest
  reset.
- A 400 about a bad parameter goes back to the client again. Bodies such
  as "Unsupported parameter: 'temperature' is not supported with this
  model." were read as "model not available here", so the request was
  tried on every upstream and the client got a 503 instead of the 400.
  Only `model_not_found`, the Codex "model is not supported when using
  Codex" reply and "The model `x` does not exist" count now.
- `most_quota` no longer ranks API keys by their per-minute rate counters.
  Those refill every minute and read close to 100%, so a pay-per-token key
  outranked every subscription. Counter windows are now marked
  `rate: true` and are not quota: an upstream with only those goes after
  every upstream that reports quota left.

## v0.2.0 (2026-10-25)

### Added

- **Codex ChatGPT login upstream** (`auth: chatgpt`). Point `token_file` at
  the Codex CLI's `auth.json`. tokenpool calls the ChatGPT Codex backend
  with the same endpoint and headers as the Codex CLI, refreshes the login
  when the access token expires or is rejected, and writes the new tokens
  back to the file (atomically, mode 0600). Requests are shaped the way
  that backend wants them, and its stream is collected for clients that
  did not ask to stream.
- **Picking a model.** Upstreams that serve the requested model as is come
  before upstreams that translate it to another model, among equal
  priority. `x-tokenpool-pin: true` (or `pin_models: true`) keeps a
  request on those native upstreams only. `model_aliases` gives models
  short names.
- **Quota tracking.** Each upstream's remaining quota is read from the
  headers it already sends: Claude's unified 5-hour and weekly limits,
  Codex's primary and secondary windows, and API-key remaining/limit
  counters. Shown per upstream in `GET /admin/upstreams` and as a summary
  in the new `GET /admin/status`.
- **Routing strategies** `default` (one upstream takes all it can serve
  until limited) and `most_quota` (the upstream with the most quota left
  right now), next to `priority` and `round_robin`.
- **Admin API**: `PUT /admin/strategy`, `POST /admin/upstreams/{name}/default`,
  `DELETE /admin/default`, `POST /admin/upstreams/{name}/priority`, and
  `pause` / `resume` as names for `disable` / `enable`. These changes are
  saved to `pool_file`, for upstreams from the config file too.

### Changed

- `strategy: failover` is now called `priority`. `failover` still works.
- Among upstreams of equal priority, native upstreams now go before
  translating ones. Give a translating upstream a lower priority to keep
  it first.
- Pausing an upstream from the config file is saved to `pool_file` and
  survives a restart. It used to last until restart.
- A usage window that is used up (a weekly limit, say) now sets the
  cooldown, even when a shorter window resets sooner. Codex's
  `resets_in_seconds` in a 429 body is read too.
- A 400 saying the upstream does not support the model (`model_not_found`)
  now tries the next upstream instead of failing the request.

## v0.1.0 (2026-10-06)

First release: failover across Claude API keys, Claude setup-tokens, Grok
API keys and Grok logins, Anthropic and OpenAI Responses APIs on the front
with translation between them, cooldowns from rate-limit headers, client
keys and a runtime admin API.
