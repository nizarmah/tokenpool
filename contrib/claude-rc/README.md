# Claude Code Remote Control through tokenpool

`claude rc` (Remote Control) will not start with a custom `ANTHROPIC_BASE_URL`:

```
Error: Remote Control is only available when using Claude via api.anthropic.com.
```

Remote Control needs your claude.ai login for its own calls (registering the
machine, polling for sessions, streaming them to the app). Only the model
calls can move. So keep `api.anthropic.com` and put a small proxy in front of
it. The proxy sends `POST /v1/messages` to tokenpool with your tokenpool key
and passes everything else to Anthropic untouched.

```
claude rc ──► mitmproxy ──► /v1/messages ──► tokenpool ──► your pool
                   └──────► everything else ──► api.anthropic.com
```

Your claude.ai login token is dropped from the model calls and never reaches
tokenpool's upstreams.

## Setup

Needs [mitmproxy](https://mitmproxy.org) (`uv tool install mitmproxy` or
`pipx install mitmproxy`) and a tokenpool key saved in
`~/.config/tokenpool/key`.

1. Run `mitmdump` once to create its certificate authority in `~/.mitmproxy/`.
2. Start the proxy:

   ```sh
   TOKENPOOL_URL=http://localhost:8080 \
   mitmdump -q --listen-host 127.0.0.1 -p 8790 \
     --allow-hosts 'api[.]anthropic[.]com' \
     --set stream_large_bodies=1 \
     -s tokenpool_addon.py
   ```

   `--allow-hosts` limits interception to Anthropic. Every other host is
   tunneled as is. `stream_large_bodies=1` keeps streaming replies streaming.

3. Start Remote Control behind it:

   ```sh
   HTTPS_PROXY=http://127.0.0.1:8790 \
   NODE_EXTRA_CA_CERTS=~/.mitmproxy/mitmproxy-ca-cert.pem \
   claude rc
   ```

Sessions it starts inherit both variables, so their model calls go through
tokenpool too. Check tokenpool's log for `msg=served` lines to confirm.

## Settings

| Variable | Default | Meaning |
| --- | --- | --- |
| `TOKENPOOL_URL` | `http://localhost:8080` | Where tokenpool listens. |
| `TOKENPOOL_KEY_FILE` | `~/.config/tokenpool/key` | Your tokenpool caller key. Read on every request, so you can rotate it live. |
| `TOKENPOOL_PIN` | unset | `true` sends `x-tokenpool-pin: true`, so requests stay on upstreams that serve the model as asked (no Claude to GPT swap). |

## As systemd user services

```ini
# ~/.config/systemd/user/rc-pool-proxy.service
[Service]
Environment=TOKENPOOL_URL=http://localhost:8080
ExecStart=%h/.local/bin/mitmdump -q --listen-host 127.0.0.1 -p 8790 --allow-hosts api[.]anthropic[.]com --set stream_large_bodies=1 -s /path/to/tokenpool_addon.py
Restart=always
```

```ini
# ~/.config/systemd/user/<your rc service>.service.d/tokenpool.conf
[Unit]
Wants=rc-pool-proxy.service
After=rc-pool-proxy.service

[Service]
Environment=HTTPS_PROXY=http://127.0.0.1:8790
Environment=NODE_EXTRA_CA_CERTS=%h/.mitmproxy/mitmproxy-ca-cert.pem
```

## Traps

- Write the host pattern as `api[.]anthropic[.]com`. In a systemd unit,
  `\.` is dropped as an unknown escape, nothing gets intercepted, and model
  calls silently keep going to Anthropic.
- The addon rewrites in the `requestheaders` hook. With
  `stream_large_bodies`, the `request` hook runs after the body has already
  been sent upstream, too late to redirect it.
- Tools run inside the sessions inherit `HTTPS_PROXY`. Other hosts pass
  through untouched, but a tool calling `api.anthropic.com` directly gets
  mitmproxy's certificate and, unless it trusts it, a TLS error.
