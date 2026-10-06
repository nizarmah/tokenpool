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

- **Any URL, any credential.** Each upstream is one credential: a Claude
  Console API key, a Claude Code setup-token, a Grok API key, or a Grok
  login. List as many as you have. The API it speaks is `anthropic` or
  `openai`.
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
| `url`        | API base. `anthropic`: like the Anthropic SDK, no `/v1` (`https://api.anthropic.com`). `openai`: like the OpenAI SDK, with `/v1` (`https://api.x.ai/v1`); a bare host gets `/v1`. A URL that already ends in `/v1/messages` or `/chat/completions` is used as is. |
| `format`     | `anthropic` (Messages API) or `openai` (Chat Completions). |
| `token`      | The upstream credential: an API key, a Claude Code setup-token, or a Grok session token. |
| `token_file` | Read the credential from this file instead, re-read whenever the file changes. `~` is your home directory. See [Keys from a file](#keys-from-a-file). |
| `token_field` | Dot path to the credential inside a JSON `token_file`, such as `tokens.access_token`. For a Grok `auth.json` with several logins, the session name instead. |
| `auth`       | `x-api-key` (default for an Anthropic API key), `setup-token` (default for a `sk-ant-oat` token), `grok` (default for a Grok `auth.json`), `bearer` (default for other OpenAI-format upstreams), `header:<Name>`, or `none`. |
| `model`      | Replaces the requested model. With `models` set, it covers models that match no pattern. |
| `models`     | Requested model to upstream model. `*` is a wildcard and the most specific pattern wins. An empty value passes the model through. With `models` set and no `model`, the upstream only serves models that match. |
| `max_tokens` | Caps `max_tokens` for this upstream. |
| `headers`    | Extra request headers, for gateways that want them. |
| `priority`   | Lower is tried first. Ties go in file order. |
| `fallback`   | `true` keeps the upstream in reserve: it's tried only after every non-fallback upstream, whatever its priority. Several fallbacks are tried by priority among themselves. |
| `disabled`   | Keeps the entry in the pool but sends it no traffic. |

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
endorsed by Anthropic or xAI. Claude and Grok are trademarks of their
owners.
