# tokenpool

[![ci](https://github.com/nizarmah/tokenpool/actions/workflows/ci.yml/badge.svg)](https://github.com/nizarmah/tokenpool/actions/workflows/ci.yml)

Pool your LLM API keys and logins behind one endpoint. When a key hits its
rate limit, runs out of credits or its provider goes down, tokenpool sends
the request to the next upstream instead: another Claude key, a Codex
(ChatGPT) login, Grok, or any API that speaks the Anthropic Messages or
OpenAI Responses API. The client gets one normal answer and never sees the
switch.

```
 Claude Code / SDKs ──► tokenpool ──► claude-primary    (429: back in 3m)
   one key per caller        │
                             ├──────► claude-secondary  ◄── served here
                             └──────► grok              (fallback)
```

- **Any URL, any credential.** Each upstream is one credential: a Claude
  Console API key, a Claude Code setup-token, a Codex CLI ChatGPT login, a
  Grok API key, or a Grok login. List as many as you have. The API it
  speaks is `anthropic` (Messages) or `openai` (Responses).
- **Both APIs on the front.** Clients call `POST /v1/messages` (Anthropic)
  or `POST /v1/responses` (OpenAI). When the upstream speaks the
  other API, tokenpool translates the request, the response and the stream,
  tool calls included. Claude Code keeps working even when Grok answers.
- **Limit-aware failover.** Cooldowns come from `Retry-After` and the
  rate-limit reset headers (Anthropic, OpenAI and xAI formats), so a
  benched upstream returns as soon as its window resets.
- **A fallback you choose.** Mark an upstream `fallback: true` and it only
  takes traffic while every other upstream is limited or down.
- **Model mapping.** Clients ask for `claude-sonnet-5`; a Grok upstream
  maps that to a Grok model.
- **Pick the model.** A request for a model goes to the upstreams that
  serve it as is first. Pin it (`x-tokenpool-pin: true`) and it never gets
  swapped for another model. Aliases give models short names.
- **Quota-aware routing.** tokenpool reads each upstream's remaining quota
  (Claude's 5-hour and weekly limits, Codex's, API-key counters) from the
  headers it already sends. Route by priority, to one default upstream, or
  to whichever upstream has the most quota left right now.
- **Caller keys.** Every person or app gets its own tokenpool key. Upstream
  keys stay on the server and never reach clients.
- **Caller priority.** Rank callers low, normal or high, and keep a paid
  API key for the ones that matter. A low caller is never sent to an
  upstream that asks for more.
- **Runtime admin API.** Add, update, pause or remove upstreams, change
  priorities, the default and the strategy, without a restart.

One static binary, one YAML file, no database.

## Install

Download the archive for your platform from
[Releases](https://github.com/nizarmah/tokenpool/releases/latest)
(Linux, macOS and Windows, amd64 and arm64), check it against
`checksums.txt`, and put `tokenpool` on your `PATH`:

```sh
os=linux arch=amd64   # or darwin / arm64
curl -fsSLO "https://github.com/nizarmah/tokenpool/releases/latest/download/tokenpool_${os}_${arch}.tar.gz"
tar -xzf "tokenpool_${os}_${arch}.tar.gz"
sudo install "tokenpool_${os}_${arch}/tokenpool" /usr/local/bin/
```

Or with Go 1.23 or newer:

```sh
go install github.com/nizarmah/tokenpool/cmd/tokenpool@latest
```

Or from a clone: `go build -o tokenpool ./cmd/tokenpool`, or
`docker build -t tokenpool .` (see [Running it](#running-it)).

## Quick start

```sh
cp tokenpool.example.yaml tokenpool.yaml
chmod 600 tokenpool.yaml        # it will hold your API keys
tokenpool keygen                # run once per caller, and once for the admin key
$EDITOR tokenpool.yaml          # replace every <...> value
tokenpool -check                # validate and print the pool
tokenpool                       # serve on :8080
```

tokenpool refuses to start while a `<...>` placeholder is left, or when a
caller or admin key is shorter than 16 characters. It warns when other
users can read the config file.

### Point clients at it

Claude Code:

```sh
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_AUTH_TOKEN=tp-...   # your tokenpool key
claude
```

Anthropic SDK: `Anthropic(base_url="http://localhost:8080", api_key="tp-...")`

OpenAI SDK, through the Responses API (`client.responses.create`):
`OpenAI(base_url="http://localhost:8080/v1", api_key="tp-...")`

Callers send their tokenpool key as `x-api-key` or `Authorization: Bearer`.
Every response carries `X-Tokenpool-Upstream`, which names the upstream
that answered.

| Endpoint | API | Notes |
| --- | --- | --- |
| `POST /v1/messages` | Anthropic Messages | Streaming and tools |
| `POST /v1/messages/count_tokens` | Anthropic | Counted by an Anthropic upstream, or estimated when none is free |
| `POST /v1/responses` | OpenAI Responses | Streaming and tools. Also at `/responses` |
| `GET /v1/models` | Both | Model names the config mentions |
| `GET /healthz` | None | Needs no key |

## `claude-pool`: Claude Code through tokenpool

`claude-pool` is a small shell function that starts Claude Code against
tokenpool, while plain `claude` keeps your usual login. It has to live in
your shell: Claude Code ignores `ANTHROPIC_BASE_URL` and
`ANTHROPIC_AUTH_TOKEN` in a project's `.claude/settings.json` and
`.claude/settings.local.json`
([settings reference](https://code.claude.com/docs/en/settings-reference#variables-claude-code-ignores-in-env)).

First save your tokenpool key where only you can read it:

```sh
install -D -m 600 /dev/null ~/.config/tokenpool/key
$EDITOR ~/.config/tokenpool/key    # paste your tp- key
```

Then add the variant that matches your setup to `~/.bashrc` (or
`~/.bash_aliases`, or `~/.zshrc`) and open a new shell.

**tokenpool on this machine:**

```bash
claude-pool() {
  local url=http://127.0.0.1:8080
  curl -fs -m 3 "$url/healthz" >/dev/null ||
    { echo "claude-pool: no tokenpool answering at $url" >&2; return 1; }
  ANTHROPIC_BASE_URL="$url" \
  ANTHROPIC_AUTH_TOKEN="$(cat ~/.config/tokenpool/key)" \
    command claude "$@"
}
```

**tokenpool on a server, through an SSH tunnel.** Keep tokenpool on the
server's loopback (`listen: "127.0.0.1:8080"`, or `-p 127.0.0.1:8080:8080`
in Docker) so only SSH reaches it. This `claude-pool` opens the tunnel the
first time you run it and leaves it up for later sessions. Set `server` to
your SSH login:

```bash
claude-pool() {
  local server=you@your-server url=http://127.0.0.1:18080
  if ! curl -fs -m 3 "$url/healthz" >/dev/null; then
    ssh -f -N -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 \
      -L 127.0.0.1:18080:127.0.0.1:8080 "$server" &&
      curl -fs -m 5 "$url/healthz" >/dev/null ||
      { echo "claude-pool: can't reach tokenpool through $server" >&2; return 1; }
  fi
  ANTHROPIC_BASE_URL="$url" \
  ANTHROPIC_AUTH_TOKEN="$(cat ~/.config/tokenpool/key)" \
    command claude "$@"
}
```

Close the tunnel with `pkill -f 'L 127.0.0.1:18080:'`; the next
`claude-pool` opens it again.

**Only in some directories.** To keep `claude-pool` to one project tree,
put this at the top of the function:

```bash
  case "$PWD/" in
    "$HOME/projects/pooled/"*) ;;
    *) echo "claude-pool: only for ~/projects/pooled" >&2; return 1 ;;
  esac
```

Inside a `claude-pool` session, `/status` shows the tokenpool URL as the
base URL, and tokenpool's log names you as the caller for each request.

## Configuration

Everything lives in one YAML file (`-config`, default `tokenpool.yaml` or
`$TOKENPOOL_CONFIG`). Unknown keys are rejected, so a typo fails loudly.
Write secrets inline, or as `${ENV_VAR}` to read them from the environment;
an unset variable stops startup. See
[`tokenpool.example.yaml`](tokenpool.example.yaml) for a complete file.

### The pool

Each entry is one credential. Add every key you want in the pool. The
example below is two Claude Console API keys, then a third Claude account
that is a setup-token, then a Grok API key held in reserve. A pool of only
setup-tokens, or only API keys, is the same shape with the other entries
left out. Same `priority` plus `strategy: round_robin` spreads load across
those entries instead of emptying the first one.

```yaml
upstreams:
  - name: claude-api-1
    url: https://api.anthropic.com
    format: anthropic
    token: sk-ant-api03-...       # Console API key
    priority: 1

  - name: claude-api-2
    url: https://api.anthropic.com
    format: anthropic
    token: sk-ant-api03-...       # another Console API key
    priority: 2

  - name: claude-setup-1
    url: https://api.anthropic.com
    format: anthropic
    token: sk-ant-oat01-...       # claude setup-token; the prefix selects it
    priority: 3

  - name: grok-api
    url: https://api.x.ai/v1
    format: openai
    token: xai-...                # console.x.ai API key
    fallback: true
    priority: 4
    models:
      "claude-opus-*": grok-4.7
    model: grok-4.5
```

Requests go to `claude-api-1`, then `claude-api-2`, then `claude-setup-1`,
and `grok-api` only once every non-fallback upstream is limited or down.
When an earlier credential's limit resets, traffic returns to it. Opus
requests that reach Grok run on `grok-4.7` and everything else on
`grok-4.5`.

A second setup-token is another upstream with its own `sk-ant-oat01-...`.
A Grok login, instead of an API key, is an upstream whose `token_file` is
`~/.grok/auth.json` (see [Grok](#grok-api-key-or-grok-login)). Mix them
freely: three setup-tokens and one API key is a normal pool.

Using a setup-token or a Grok login this way is against that provider's
terms of use. Use either at your own discretion. A Console API key or an
xAI API key is the credential those terms allow. Details are under
[Anthropic](#anthropic-api-keys-and-setup-tokens) and
[Grok](#grok-api-key-or-grok-login).

| field        | meaning |
|--------------|---------|
| `name`       | Identifier for logs, headers and the admin API. Letters, digits, `.`, `_` or `-`. |
| `url`        | API base. `anthropic`: like the Anthropic SDK, no `/v1` (`https://api.anthropic.com`). `openai`: like the OpenAI SDK, with `/v1` (`https://api.x.ai/v1`); a bare host gets `/v1`. A URL that already ends in `/v1/messages` or `/responses` is used as is. |
| `format`     | `anthropic` (Messages API) or `openai` (Responses API). |
| `token`      | The upstream credential: an API key, a Claude Code setup-token, or a Grok session token. |
| `token_file` | Read the credential from this file instead, re-read whenever the file changes. `~` is your home directory. See [Keys from a file](#keys-from-a-file). |
| `token_field` | Dot path to the credential inside a JSON `token_file`, such as `tokens.access_token`. For a Grok `auth.json` with several logins, the session name instead. |
| `auth`       | `x-api-key` (default for an Anthropic API key), `setup-token` (default for a `sk-ant-oat` token), `grok` (default for a Grok `auth.json`), `chatgpt` (default for a Codex CLI `auth.json`), `bearer` (default for other OpenAI-format upstreams), `header:<Name>`, or `none`. |
| `model`      | Replaces the requested model. With `models` set, it covers models that match no pattern. |
| `models`     | Requested model to upstream model. `*` is a wildcard and the most specific pattern wins. An empty value passes the model through: the upstream serves that model natively. With `models` set and no `model`, the upstream only serves models that match. See [Picking a model](#picking-a-model). |
| `max_tokens` | Caps `max_tokens` for this upstream. |
| `headers`    | Extra request headers, for gateways that want them. |
| `priority`   | Lower is tried first. Ties go to upstreams that serve the model natively, then file order. Change it at runtime through the [admin API](#admin-api). |
| `fallback`   | `true` keeps the upstream in reserve: it's tried only after every non-fallback upstream, whatever its priority. Several fallbacks are tried by priority among themselves. |
| `disabled`   | Keeps the entry in the pool but sends it no traffic. |
| `min_client_priority` | The lowest caller priority this upstream serves: `low` (default, everyone), `normal` or `high`. See [Caller priority](#caller-priority). |

### Keys from a file

For a token that another tool keeps refreshed, such as a secrets agent,
a mounted Kubernetes secret or a CLI's login file, point `token_file` at
it instead of writing `token`:

```yaml
  - name: gateway
    url: https://llm-gateway.example.com/v1
    format: openai
    token_file: /run/secrets/gateway-token.json
    token_field: credentials.access_token   # omit for a bare-token file
```

tokenpool checks the file on every request and re-reads it when it
changes, so a refreshed token is used right away. A file holding just the
token works as is. For JSON, tokenpool uses `token_field`, or else a
top-level `access_token`, `accessToken` or `token`. A Grok
`~/.grok/auth.json` is recognized on its own: tokenpool reads the
session's `key`. It refuses to start when the file can't be read or
parsed. Later read errors bench the upstream like any other failure, and
error messages never quote the file.

`token_file` can only be set in the config file. The admin API rejects
it, because an admin-added upstream could otherwise send any file on the
server to any URL. The token is still subject to the provider's terms, so
make sure the credential is one you're allowed to use this way.

In Docker, mount the file's directory rather than the file itself. Tools
that refresh a token usually replace the file, and a single-file mount
keeps showing the old copy.

### Top-level settings

| key | default | meaning |
| --- | --- | --- |
| `listen` | `:8080` | Address to serve on. `127.0.0.1:8080` accepts local connections only. |
| `client_keys` | required | `name: key` per caller. Names show up in the logs. |
| `client_priorities` | none | `name: low`, `normal` or `high` per caller. A caller left out is `normal`. See [Caller priority](#caller-priority). |
| `client_upstreams` | none | `name: [upstream, ...]` per caller: upstreams that caller tries first, in order. See [Caller upstreams](#caller-upstreams). |
| `allow_anonymous` | `false` | Serve requests without a key. Local testing only. |
| `admin_key` | unset (admin API off) | Key for `/admin`. Must differ from every caller key. |
| `pool_file` | unset | Where admin-added upstreams are saved. A relative path sits next to the config. |
| `strategy` | `priority` | How upstreams are ordered: `priority`, `round_robin`, `default` or `most_quota`. See [Routing strategies](#routing-strategies). `failover` is the old name of `priority`. |
| `default_upstream` | unset | The upstream the `default` strategy sends traffic to first. |
| `pin_models` | `false` | Pin every request to its model unless it sends `x-tokenpool-pin: false`. See [Picking a model](#picking-a-model). |
| `model_aliases` | none | Short names for models, such as `best-code: gpt-5-codex`. |
| `default_max_tokens` | `8192` | Filled in when a Responses request without `max_output_tokens` goes to an Anthropic upstream. |
| `max_body_bytes` | `67108864` | Largest request body accepted (64 MiB). |
| `connect_timeout` | `10s` | Time to connect to an upstream. |
| `header_timeout` | `10m` | Wait for an upstream's first response byte before failing over. |
| `cooldowns` | see below | How long an upstream sits out after each kind of failure. |

### When tokenpool fails over

| upstream answer | what happens | cooldown |
|---|---|---|
| 2xx | relayed to the client | none |
| 429 | next upstream | `Retry-After`, an exhausted usage window's reset (a weekly limit outlasts the 5-hour one), reset headers or the body's `resets_in_seconds`, else `cooldowns.rate_limit` (1m) |
| 503, 529 | next upstream | `Retry-After`, else `cooldowns.overloaded` (15s) |
| 402, or 400/403/422 saying "credit balance", "quota", "billing"… | next upstream | reset headers, else `cooldowns.quota` (1h) |
| 401, 403 | next upstream (check the key) | `cooldowns.auth` (10m) |
| other 5xx, 408, timeout, connection error | next upstream | `Retry-After`, else `cooldowns.error` (30s) |
| 404, or 400 saying the model is not supported | next upstream (that upstream lacks the model or path) | none |
| 3xx | next upstream (redirects are never followed, so keys can't leak) | none |
| other 4xx | relayed: the request itself is at fault | none |

No cooldown runs past `cooldowns.max` (24h). When no upstream can serve a
request, the client gets `429` (every upstream is limited) or `503`, with
a `Retry-After` for the soonest comeback and each upstream's reason. Claude
Code and the SDKs retry these on their own.

tokenpool can only fail over before the response starts. Once an upstream
starts streaming, the client stays on that stream to the end.

## Picking a model

An upstream serves a model **natively** when it sends the requested model
as is: it has no `models`/`model` rewriting for it, or its `models` entry
for it is empty. An upstream that maps the model to another one (Claude
requests to a Grok or Codex model) serves it by **translation**.

```yaml
upstreams:
  - name: claude-1
    url: https://api.anthropic.com
    format: anthropic
    token: sk-ant-oat01-...
    models: {"claude-*": ""}             # Claude models, natively

  - name: codex-1
    format: openai
    auth: chatgpt
    token_file: ~/.codex/auth.json
    models:
      "gpt-*": ""                        # OpenAI models, natively
      "claude-*": gpt-5-codex            # Claude requests, by translation

model_aliases:
  best-code: gpt-5-codex                 # clients can ask for "best-code"
```

- A request goes to every upstream that can serve it. Among upstreams of
  equal priority, native ones come first, so `claude-opus-*` goes to
  `claude-1` and `gpt-5-codex` goes to `codex-1`.
- **Pinned** requests only go to native upstreams, so a model you chose
  for a reason is never swapped for another. Pin one request with the
  header `x-tokenpool-pin: true`, or every request with `pin_models: true`
  (and unpin one with `x-tokenpool-pin: false`). A pinned request whose
  native upstreams are all out gets a 429 or 503, not another model.
- Unpinned requests fall over to translation when every native upstream
  is out, or earlier when you put a translating upstream first with a
  lower priority, the `default` strategy or `most_quota`.
- An alias is resolved before routing, and the upstream receives the real
  model name. `GET /v1/models` lists aliases too.

In Claude Code, pick a model with `claude --model gpt-5-codex` (or an
alias), and pin every request with
`ANTHROPIC_CUSTOM_HEADERS="x-tokenpool-pin: true"`.

In a mixed pool, give each upstream a `models` list so tokenpool knows
what it serves. An upstream without one passes every model through, and
counts as native for all of them.

## Routing strategies

`strategy` decides which upstream goes first. Fallbacks (`fallback: true`)
always come after every other upstream, and pinning always applies.

| strategy | order |
| --- | --- |
| `priority` | Lowest `priority` first. Ties: native upstreams, then file order. |
| `round_robin` | Like `priority`, but rotates among upstreams that tie, to spread load. |
| `default` | `default_upstream` takes every request it can serve, until it is limited or failing. Then `priority` order. |
| `most_quota` | The upstream with the most quota left right now, in whole percents. Ties go by `priority`, then nativeness. An upstream that has not reported its quota yet counts as full. An API key reports only per-minute rate counters, which are not quota, so it goes after every upstream that reports some. |

Use `default` to make one account the main one for a while, and
`most_quota` to always spend whichever account has the most left. If you
pay per token for an API key, still mark it `fallback: true`: then it is
used only once every subscription is limited, under every strategy. Set the
strategy in the config file, or switch at runtime:

```sh
curl -X PUT localhost:8080/admin/strategy -H "$A" -d '{"strategy":"most_quota"}'
curl -X POST localhost:8080/admin/upstreams/codex-1/default -H "$A"    # also sets strategy: default
curl -X DELETE localhost:8080/admin/default -H "$A"                    # back to priority
curl -X POST localhost:8080/admin/upstreams/claude-2/priority -H "$A" -d '{"priority":0}'
```

Changes made through the admin API are saved in `pool_file` and survive a
restart. They win over the config file until you change them again
through the API. Without a `pool_file` they last until restart.

## Caller priority

Several projects can share one pool. Some upstreams are too scarce to
share, like a paid API key. Give each caller a priority and each upstream
the lowest priority it will serve.

```yaml
client_keys:
  alice: <tokenpool keygen output>    # your own work
  shop: <tokenpool keygen output>
  batch: <tokenpool keygen output>    # bulk jobs, nobody waits on them

client_priorities:                    # low, normal or high; no entry means normal
  alice: high
  batch: low

upstreams:
  - name: claude-sub-1
    url: https://api.anthropic.com
    format: anthropic
    token: sk-ant-oat01-...
    priority: 1

  - name: claude-sub-2
    url: https://api.anthropic.com
    format: anthropic
    token: sk-ant-oat01-...
    priority: 2

  - name: codex
    format: openai
    auth: chatgpt
    token_file: ~/.config/tokenpool/codex/auth.json
    priority: 3
    models:
      "gpt-*": ""
      "claude-*": gpt-5-codex

  - name: claude-api               # paid, per token
    url: https://api.anthropic.com
    format: anthropic
    token: sk-ant-api03-...
    fallback: true
    min_client_priority: high      # only high callers
```

`alice` can use all four, and the API key only once the subscriptions and
Codex are limited. `shop` and `batch` never reach `claude-api`. `batch` is
low and `shop` is normal, so you could also set `min_client_priority:
normal` on an upstream to keep `batch` off it and let `shop` on.

The check runs first. An upstream a caller is not allowed to use is
dropped before the strategy orders anything, so it is never a failover
step or a fallback for that caller. It applies to `/v1/messages`,
`/v1/responses` and `/v1/messages/count_tokens`.

When a caller's priority leaves nothing to try, the answer says so:

- Every upstream that could serve the model is closed to that caller:
  `403` with `permission_error` (Anthropic shape on `/v1/messages`, OpenAI
  shape on `/v1/responses`) and the header `X-Tokenpool-Refused:
  caller-priority`. Waiting will not help, and the SDKs do not retry a
  `403`.
- The allowed upstreams exist but are all limited: the usual `429` or
  `503` with `Retry-After`. The message ends with `the caller's priority
  (low) excludes claude-api (needs high)`, so you can tell it from a plain
  limit.

The logs carry `priority=low` next to `client=batch` on the served and
refused lines. `tokenpool -check` prints each caller's priority and a
`MIN CALLER` column. Callers without a key (`allow_anonymous`) count as
`normal`. `client_priorities` is set in the config file only. Misspell a
name or a priority and tokenpool will not start.

Change an upstream's minimum at runtime, or set it when you add or replace
one. Config upstreams keep the change in `pool_file`, like a priority:

```sh
curl -X POST localhost:8080/admin/upstreams/claude-api/min_client_priority -H "$A" \
  -d '{"min_client_priority":"normal"}'
```

### Caller upstreams

`client_upstreams` sends one caller to the upstreams you name before the
rest of the pool, for example a chat bot that should run on Codex and keep
the Claude subscriptions for people:

```yaml
client_upstreams:
  bot: [codex]                     # Codex first; the pool's usual order after it
```

The named upstreams go first, in the order listed. Every other upstream
the caller can use follows in the strategy's order, so when `codex` is
limited the bot fails over to Claude as usual. It only reorders:
`min_client_priority`, pinning and model maps still decide what the caller
can reach, and a name that matches no upstream is skipped. `/admin/status`
lists each caller's upstreams. Set it in the config file only.

## Quota tracking

Every response from an upstream carries its usage limits in headers.
tokenpool keeps the latest reading per upstream:

| Provider | Headers | Windows |
| --- | --- | --- |
| Claude subscription (setup-token) | `anthropic-ratelimit-unified-5h-utilization`, `-7d-utilization`, their `-reset` and `-status` | `5h`, `7d` (and model-specific ones such as `7d_opus`) |
| Codex (ChatGPT login) | `x-codex-primary-used-percent`, `-window-minutes`, `-reset-at`, and the same for `secondary` | `codex.primary`, `codex.secondary` |
| Anthropic API key | `anthropic-ratelimit-{requests,tokens,input-tokens,output-tokens}-{remaining,limit,reset}` | `requests`, `tokens`, ... (`rate: true`) |
| OpenAI, xAI and other API keys | `x-ratelimit-{remaining,limit,reset}-{requests,tokens}` | `requests`, `tokens` (`rate: true`) |

An upstream's remaining quota is its smallest remaining share across the
windows that apply to every model. A window whose reset time has passed
counts as full again. Model-specific windows (`model_specific: true`) and
API-key rate counters (`rate: true`, which refill every minute) are shown
but not counted. A rate limit that hits only one model, such as Claude's
weekly Opus limit, does not bench the whole upstream until that limit
resets: the upstream waits for its soonest reset instead, so other models
keep working.
Read it in `GET /admin/upstreams` (each upstream's `quota`) or as a
summary in `GET /admin/status`:

```json
{
  "strategy": "most_quota",
  "first": "codex-1",
  "upstreams": [
    {"name": "codex-1", "state": "available", "priority": 1,
     "remaining_percent": 65, "window": "codex.secondary", "resets_at": "2026-10-29T08:00:00Z"},
    {"name": "claude-2", "state": "available", "priority": 1,
     "remaining_percent": 25, "window": "7d", "resets_at": "2026-10-28T10:00:00Z"},
    {"name": "claude-1", "state": "cooling", "priority": 0,
     "remaining_percent": 0, "window": "7d", "resets_at": "2026-10-27T10:00:00Z",
     "cooldown_until": "2026-10-27T10:00:00Z", "cooldown_reason": "rate limited"}
  ],
  "pin_models": false
}
```

`first` is where a request that every upstream can serve would go right
now. When a limit runs out, the upstream sits out until that window
resets: a Claude account out of its weekly limit is not retried every five
hours.

## Admin API

Set `admin_key` to turn it on, and send that key as `x-api-key` or a bearer
token. Upstreams you add here go in `pool_file` (mode 0600), so they survive
restarts. Upstreams from the config file can be paused, resumed, reset,
reprioritized or made the default here, but you edit the rest in the file.

```sh
A="Authorization: Bearer $TOKENPOOL_ADMIN_KEY"

# Add any URL with any key
curl -X POST localhost:8080/admin/upstreams -H "$A" -d '{
  "name": "claude-third", "url": "https://api.anthropic.com",
  "format": "anthropic", "token": "sk-ant-api03-...", "priority": 3
}'

curl localhost:8080/admin/upstreams -H "$A"                              # the pool, keys redacted
curl -X PUT localhost:8080/admin/upstreams/claude-third -H "$A" -d '{...}' # replace (omit token to keep it)
curl -X POST localhost:8080/admin/upstreams/claude-primary/reset -H "$A"   # clear a cooldown
curl -X POST localhost:8080/admin/upstreams/claude-primary/pause -H "$A"   # stop sending it traffic (also: resume)
curl -X DELETE localhost:8080/admin/upstreams/claude-third -H "$A"

curl localhost:8080/admin/status -H "$A"                                 # strategy, order, quota left, callers and their priority
curl -X POST localhost:8080/admin/upstreams/claude-third/priority -H "$A" -d '{"priority":0}'
curl -X POST localhost:8080/admin/upstreams/claude-third/min_client_priority -H "$A" -d '{"min_client_priority":"high"}'
curl -X POST localhost:8080/admin/upstreams/claude-third/default -H "$A"  # make it the main one
curl -X DELETE localhost:8080/admin/default -H "$A"
curl -X PUT localhost:8080/admin/strategy -H "$A" -d '{"strategy":"most_quota"}'
```

`pause` and `resume` are also `disable` and `enable`. Pauses, priorities,
minimum caller priorities, the default and the strategy are saved to `pool_file`, for upstreams from
the config file too, so they survive a restart.

## Translation notes

When client and upstream speak the same API, the body passes through
untouched apart from `model` and the `max_tokens` (or `max_output_tokens`)
cap. Anthropic-only fields (`thinking`, `cache_control`, betas) keep
working against Claude.

Across APIs, tokenpool translates text, images, system prompts, tools,
tool calls and results, usage and streaming. Some features have no
equivalent on the other side, so they're dropped:

- Anthropic → Responses upstream: extended thinking, prompt-cache controls,
  server tools (web search, code execution), PDF documents and stop
  sequences. Tool-result images go in the next user message. tokenpool
  sends `store: false`: the next turn may go to another upstream, so
  nothing stored would be used.
- Responses → Anthropic upstream: reasoning items, built-in tools,
  `text.format`, `logprobs`, and files other than inline PDFs.
  `temperature` is capped at 1, and `max_tokens` defaults to
  `default_max_tokens` (8192) because Anthropic requires one.
- tokenpool is stateless. Send the whole conversation in `input` each
  turn: a request with `previous_response_id`, `conversation` or an
  `item_reference` skips Anthropic upstreams, and only the Responses
  upstream that stored that response can serve it.
- In translated streams, text streams live, but each tool call arrives
  whole when it finishes. (Responses providers may interleave parallel
  calls, and Anthropic's stream format can't express that.)

`/v1/messages/count_tokens` goes to an Anthropic upstream. If none is
available, tokenpool estimates the count (about 4 bytes per token) and
sets `X-Tokenpool-Estimated: true`.

## Running it

Docker, with the config file holding the keys:

```sh
docker build -t tokenpool .
mkdir -p data
docker run -d --name tokenpool --restart unless-stopped \
  --user "$(id -u):$(id -g)" \
  -p 127.0.0.1:8080:8080 \
  -v "$PWD/tokenpool.yaml:/etc/tokenpool/tokenpool.yaml:ro" \
  -v "$PWD/data:/data" \
  tokenpool
```

`--user` runs it as you, so it can read your `chmod 600` config and write
to `data/`. Keep `listen: ":8080"` inside the container, and set
`pool_file: /data/pool.json` so admin changes land in `data/`. The image
logs JSON.

tokenpool serves plain HTTP, and keys travel in headers. Keep it on
loopback or a private network, or put a TLS proxy in front. To reach a
loopback-only tokenpool on a server from your laptop, use the SSH-tunnel
[`claude-pool`](#claude-pool-claude-code-through-tokenpool).

Logs record the caller name, model, upstream, failovers and timing for each
request, and never keys.

## Anthropic: API keys and setup-tokens

An Anthropic upstream takes either kind of credential, and the pool can
hold any number of each. Leave `auth` unset and the token decides, or set
it. Two API keys and two setup-tokens are four upstreams:

```yaml
  - name: claude-api-1
    url: https://api.anthropic.com
    format: anthropic
    token: sk-ant-api03-...
    priority: 1

  - name: claude-api-2
    url: https://api.anthropic.com
    format: anthropic
    token: sk-ant-api03-...
    priority: 1          # same priority: round_robin shares these two

  - name: claude-setup-1
    url: https://api.anthropic.com
    format: anthropic
    token: sk-ant-oat01-...    # from: claude setup-token
    priority: 2

  - name: claude-setup-2
    url: https://api.anthropic.com
    format: anthropic
    token: sk-ant-oat01-...
    auth: setup-token          # optional; the sk-ant-oat prefix selects this
    priority: 2
```

| Credential | `auth` | How it is sent |
| --- | --- | --- |
| Console API key, `sk-ant-api03-...` | `x-api-key` (the default) | `x-api-key` |
| Setup-token from `claude setup-token`, `sk-ant-oat01-...` | `setup-token` (the default for this prefix) | `Authorization: Bearer`, plus `oauth-2025-04-20` on `anthropic-beta` |

`auth: x-api-key` forces the API-key header even when the token starts with
`sk-ant-oat`. `auth: setup-token` forces bearer auth for a token that does
not have that prefix, including one read from `token_file`.

For a setup-token, tokenpool also forwards the identity headers the client
actually sent (`User-Agent`, `x-app`, `anthropic-dangerous-direct-browser-access`,
and `x-stainless-*`). It does not fill those in itself. Point Claude Code at
tokenpool with [`claude-pool`](#claude-pool-claude-code-through-tokenpool) so
the request is a Claude Code request and only the credential is swapped.

Using a setup-token through tokenpool is against
[Anthropic's terms](https://code.claude.com/docs/en/legal-and-compliance).
OAuth tokens from `claude setup-token` are only for that subscriber's own
use of Claude Code and Anthropic's other apps. Anthropic does not permit
storing those tokens or routing other requests through a Free, Pro, or Max
credential, and may enforce that without notice. Use one here at your own
discretion. A Console API key is the credential those terms allow for any
other caller.

## Codex: ChatGPT login

A ChatGPT plan's Codex quota is an upstream too. Sign in with the
[Codex CLI](https://github.com/openai/codex) and point `token_file` at the
`auth.json` it writes:

```sh
CODEX_HOME=~/.config/tokenpool/codex codex login --device-auth   # a login only tokenpool uses
```

```yaml
  - name: codex-1
    format: openai
    auth: chatgpt                          # optional: a Codex auth.json selects it
    token_file: ~/.config/tokenpool/codex/auth.json
    # url defaults to https://chatgpt.com/backend-api/codex
    models:
      "gpt-*": ""                          # the models your plan lists
      "claude-*": gpt-5-codex              # optional: take Claude requests too
```

tokenpool calls the endpoint the Codex CLI calls,
`https://chatgpt.com/backend-api/codex/responses`, with the same headers:
`Authorization: Bearer` with the login's access token, `ChatGPT-Account-ID`,
`originator: codex_cli_rs` and a `codex_cli_rs/...` User-Agent. That
backend only streams, never stores, takes `input` only as a list and
rejects `max_output_tokens` and `temperature`, so tokenpool shapes requests
that way and collects the stream into one response for clients that did
not ask to stream.

When the access token is about to expire, or the backend rejects it,
tokenpool refreshes the login at `auth.openai.com` the way the Codex CLI
does, and writes the new tokens back to `auth.json` (atomically, mode
0600). The refresh token changes on every refresh, so give tokenpool its
own login (its own `CODEX_HOME`) rather than the one your `codex` CLI uses:
two programs refreshing one login log each other out. If the refresh
token stops working, the error says so; run `codex login` again.

The models a ChatGPT login can use depend on the plan and change over
time; the backend answers `model_not_found` for others, and tokenpool then
tries the next upstream. `codex` lists them in its `/model` picker. Codex's
usage limits show up in [quota tracking](#quota-tracking).

A Codex `auth.json` from an API-key login (`OPENAI_API_KEY`) is read as a
plain bearer key; point that upstream at `https://api.openai.com/v1`.

A ChatGPT login is a personal account credential. OpenAI's
[terms of use](https://openai.com/policies/terms-of-use/) say not to share
account credentials or make your account available to anyone else. Use a
login here at your own discretion; an OpenAI API key is the credential
for serving other people.

## Grok: API key or Grok login

A Grok upstream takes either an API key from
[console.x.ai](https://console.x.ai) or the session `grok login` stores in
`~/.grok/auth.json`. Use one, or several of each. An API key talks to the
public API. A login file talks to Grok's CLI chat proxy, which is what the
`grok` command itself calls.

```yaml
  - name: grok-api-1
    url: https://api.x.ai/v1
    format: openai
    token: xai-...
    priority: 1
    models:
      "claude-*": grok-4.5

  - name: grok-api-2
    url: https://api.x.ai/v1
    format: openai
    token: xai-...
    priority: 2
    model: grok-4.5

  - name: grok-login
    url: https://cli-chat-proxy.grok.com/v1
    format: openai
    token_file: ~/.grok/auth.json   # written by `grok login`
    priority: 3
    model: grok-4.5
```

| Credential | `auth` | Where it goes |
| --- | --- | --- |
| API key, `xai-...` | `bearer` (the default) | `https://api.x.ai/v1`, as `Authorization: Bearer` |
| `~/.grok/auth.json` | `grok` (the default when the file is a Grok login) | `https://cli-chat-proxy.grok.com/v1` |

For a login file, tokenpool sends the session `key` as
`Authorization: Bearer` and sets `X-XAI-Token-Auth: xai-grok-cli`. It also
sets `x-grok-model-override` to the model it actually sends (after `model`
/ `models`), and `x-grok-client-version`: the proxy answers 426 ("Your
Grok CLI version (none) is outdated") without a recent one. When it raises
that floor, set the version under the upstream's `headers`. `auth: grok` forces that even for a token written inline.
`auth: bearer` forces a plain API-key request and does not add the CLI
headers.

`grok login` keeps `auth.json` fresh. tokenpool does not refresh the
session itself; it re-reads the file when it changes. In Docker, mount the
directory that contains `auth.json`, not the file: `grok login` replaces
it (see [Keys from a file](#keys-from-a-file)).

A file with more than one login needs `token_field` set to the session
name, the object's top-level key (`tokenpool -check` names them). Two
logins are two upstreams, each naming one session. Most models on the CLI
proxy only stream; Claude Code already streams.

Using a Grok login through tokenpool is against
[SpaceXAI's terms of service](https://x.ai/legal/terms-of-service).
`~/.grok/auth.json` is an account credential, and those terms say not to
share account credentials or make the account available to anyone else.
The developer API, which an `xai-...` key calls, is a separate agreement.
Use a login file here at your own discretion.

## Contributing

Issues and pull requests are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md).
Report security problems privately, as [SECURITY.md](SECURITY.md) describes.

## License

[MIT](LICENSE). tokenpool is an independent project, not affiliated with or
endorsed by Anthropic, OpenAI or xAI. Claude, Codex, ChatGPT and Grok are
trademarks of their owners.
