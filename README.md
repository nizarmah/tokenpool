# tokenpool

[![ci](https://github.com/nizarmah/tokenpool/actions/workflows/ci.yml/badge.svg)](https://github.com/nizarmah/tokenpool/actions/workflows/ci.yml)

Pool your LLM API keys behind one endpoint. When a key hits its rate limit,
runs out of credits or its provider goes down, tokenpool sends the request
to the next upstream instead: another Claude key, Grok, or any API that
speaks the Anthropic or OpenAI format. The client gets one normal answer
and never sees the switch.

```
 Claude Code / SDKs ──► tokenpool ──► claude-primary    (429: back in 3m)
   one key per caller        │
                             ├──────► claude-secondary  ◄── served here
                             └──────► grok              (fallback)
```

- **Any URL, any key.** Each upstream is a URL, a key, the API it speaks
  (`anthropic` or `openai`) and how it wants the key sent.
- **Both APIs on the front.** Clients call `POST /v1/messages` (Anthropic)
  or `POST /v1/chat/completions` (OpenAI). When the upstream speaks the
  other API, tokenpool translates the request, the response and the stream,
  tool calls included. Claude Code keeps working even when Grok answers.
- **Limit-aware failover.** Cooldowns come from `Retry-After` and the
  rate-limit reset headers (Anthropic, OpenAI and xAI formats), so a
  benched upstream returns as soon as its window resets.
- **A fallback you choose.** Mark an upstream `fallback: true` and it only
  takes traffic while every other upstream is limited or down.
- **Model mapping.** Clients ask for `claude-sonnet-5`; a Grok upstream
  maps that to a Grok model.
- **Caller keys.** Every person or app gets its own tokenpool key. Upstream
  keys stay on the server and never reach clients.
- **Runtime admin API.** Add, update, pause or remove upstreams without a
  restart.

One static binary, one YAML file, no database.

## Install

With Go 1.23 or newer:

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

OpenAI SDK, and anything OpenAI-compatible:
`OpenAI(base_url="http://localhost:8080/v1", api_key="tp-...")`

Callers send their tokenpool key as `x-api-key` or `Authorization: Bearer`.
Every response carries `X-Tokenpool-Upstream`, which names the upstream
that answered.

| Endpoint | API | Notes |
| --- | --- | --- |
| `POST /v1/messages` | Anthropic Messages | Streaming and tools |
| `POST /v1/messages/count_tokens` | Anthropic | Counted by an Anthropic upstream, or estimated when none is free |
| `POST /v1/chat/completions` | OpenAI Chat Completions | Also at `/chat/completions` |
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

```yaml
upstreams:
  - name: claude-primary
    url: https://api.anthropic.com
    format: anthropic
    token: sk-ant-api03-...
    priority: 1

  - name: claude-secondary
    url: https://api.anthropic.com
    format: anthropic
    token: sk-ant-api03-...
    priority: 2

  - name: grok
    url: https://api.x.ai/v1
    format: openai
    token: xai-...
    fallback: true
    priority: 3
    models:
      "claude-opus-*": grok-4.7
    model: grok-4.5
```

Requests go to `claude-primary`, then `claude-secondary` when it's
limited, then `grok` once both are. When a Claude key's limit resets,
traffic returns to it. Opus requests run on `grok-4.7` and everything else
on `grok-4.5`.

| field        | meaning |
|--------------|---------|
| `name`       | Identifier for logs, headers and the admin API. Letters, digits, `.`, `_` or `-`. |
| `url`        | API base. `anthropic`: like the Anthropic SDK, no `/v1` (`https://api.anthropic.com`). `openai`: like the OpenAI SDK, with `/v1` (`https://api.x.ai/v1`); a bare host gets `/v1`. A URL that already ends in `/v1/messages` or `/chat/completions` is used as is. |
| `format`     | `anthropic` (Messages API) or `openai` (Chat Completions). |
| `token`      | The upstream's API key. |
| `auth`       | `x-api-key` (default for anthropic), `bearer` (default for openai), `header:<Name>`, or `none`. |
| `model`      | Replaces the requested model. With `models` set, it covers models that match no pattern. |
| `models`     | Requested model to upstream model. `*` is a wildcard and the most specific pattern wins. An empty value passes the model through. With `models` set and no `model`, the upstream only serves models that match. |
| `max_tokens` | Caps `max_tokens` for this upstream. |
| `headers`    | Extra request headers, for gateways that want them. |
| `priority`   | Lower is tried first. Ties go in file order. |
| `fallback`   | `true` keeps the upstream in reserve: it's tried only after every non-fallback upstream, whatever its priority. Several fallbacks are tried by priority among themselves. |
| `disabled`   | Keeps the entry in the pool but sends it no traffic. |

### Top-level settings

| key | default | meaning |
| --- | --- | --- |
| `listen` | `:8080` | Address to serve on. `127.0.0.1:8080` accepts local connections only. |
| `client_keys` | required | `name: key` per caller. Names show up in the logs. |
| `allow_anonymous` | `false` | Serve requests without a key. Local testing only. |
| `admin_key` | unset (admin API off) | Key for `/admin`. Must differ from every caller key. |
| `pool_file` | unset | Where admin-added upstreams are saved. A relative path sits next to the config. |
| `strategy` | `failover` | `round_robin` spreads load across upstreams that share a priority. Later priorities and fallbacks still only take traffic when those are out. |
| `default_max_tokens` | `8192` | Filled in when an OpenAI-style request without `max_tokens` goes to an Anthropic upstream. |
| `max_body_bytes` | `67108864` | Largest request body accepted (64 MiB). |
| `connect_timeout` | `10s` | Time to connect to an upstream. |
| `header_timeout` | `10m` | Wait for an upstream's first response byte before failing over. |
| `cooldowns` | see below | How long an upstream sits out after each kind of failure. |

### When tokenpool fails over

| upstream answer | what happens | cooldown |
|---|---|---|
| 2xx | relayed to the client | none |
| 429 | next upstream | `Retry-After` / reset headers, else `cooldowns.rate_limit` (1m) |
| 503, 529 | next upstream | `Retry-After`, else `cooldowns.overloaded` (15s) |
| 402, or 400/403/422 saying "credit balance", "quota", "billing"… | next upstream | reset headers, else `cooldowns.quota` (1h) |
| 401, 403 | next upstream (check the key) | `cooldowns.auth` (10m) |
| other 5xx, 408, timeout, connection error | next upstream | `Retry-After`, else `cooldowns.error` (30s) |
| 404 | next upstream (that upstream lacks the model or path) | none |
| 3xx | next upstream (redirects are never followed, so keys can't leak) | none |
| other 4xx | relayed: the request itself is at fault | none |

No cooldown runs past `cooldowns.max` (24h). When no upstream can serve a
request, the client gets `429` (every upstream is limited) or `503`, with
a `Retry-After` for the soonest comeback and each upstream's reason. Claude
Code and the SDKs retry these on their own.

tokenpool can only fail over before the response starts. Once an upstream
starts streaming, the client stays on that stream to the end.

## Admin API

Set `admin_key` to turn it on, and send that key as `x-api-key` or a bearer
token. Upstreams you add here go in `pool_file` (mode 0600), so they survive
restarts. Upstreams from the config file can be disabled, enabled or reset
here, but you edit them in the file.

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
curl -X POST localhost:8080/admin/upstreams/claude-primary/disable -H "$A" # pause (also: enable)
curl -X DELETE localhost:8080/admin/upstreams/claude-third -H "$A"
```

## Translation notes

When client and upstream speak the same API, the body passes through
untouched apart from `model` and the `max_tokens` cap. Anthropic-only
fields (`thinking`, `cache_control`, betas) keep working against Claude.

Across APIs, tokenpool translates text, images, system prompts, tools,
tool calls and results, stop sequences, usage and streaming. Some features
have no equivalent on the other side, so they're dropped:

- Anthropic → OpenAI upstream: extended thinking, prompt-cache controls,
  server tools (web search, code execution), PDF documents. Tool-result
  images go in the next user message.
- OpenAI → Anthropic upstream: `n`, `response_format`, `logprobs`, audio.
  `temperature` is capped at 1, and `max_tokens` defaults to
  `default_max_tokens` (8192) because Anthropic requires one.
- In translated streams, text streams live, but each tool call arrives
  whole when it finishes. (OpenAI providers may interleave parallel
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

## Use API keys, not subscription tokens

Use a Claude Console API key (`sk-ant-api...`) for Claude upstreams, not a
subscription token from `claude setup-token`. Anthropic's terms reserve
Free/Pro/Max OAuth tokens for the subscriber's own use of Claude Code and
Anthropic's apps. Routing other people's requests through one isn't
allowed, and Anthropic may cut off the account
([Claude Code legal and compliance](https://code.claude.com/docs/en/legal-and-compliance)).
API keys that your organization provisions for its own staff are fine.
Check every other provider's terms the same way.

## Contributing

Issues and pull requests are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md).
Report security problems privately, as [SECURITY.md](SECURITY.md) describes.

## License

[MIT](LICENSE). tokenpool is an independent project, not affiliated with or
endorsed by Anthropic or xAI. Claude and Grok are trademarks of their
owners.
