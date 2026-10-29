"""mitmproxy addon: send Claude Code model calls to tokenpool, pass everything else through.

Claude Code's Remote Control refuses a custom ANTHROPIC_BASE_URL, so the rc
process keeps api.anthropic.com and runs behind this proxy instead. Only
POST /v1/messages* is rewritten: the claude.ai login header is dropped and the
tokenpool caller key is sent. Sessions, auth and everything else go to
Anthropic untouched.

Env: TOKENPOOL_URL (default http://localhost:8080),
TOKENPOOL_KEY_FILE (default ~/.config/tokenpool/key),
TOKENPOOL_PIN=true to keep requests on real Claude upstreams.
"""
import os
from urllib.parse import urlsplit

from mitmproxy import http

_url = urlsplit(os.environ.get("TOKENPOOL_URL", "http://localhost:8080"))
_key_file = os.path.expanduser(os.environ.get("TOKENPOOL_KEY_FILE", "~/.config/tokenpool/key"))
_pin = os.environ.get("TOKENPOOL_PIN", "")


def requestheaders(flow: http.HTTPFlow) -> None:
    # Runs before the body streams, so the rewrite also applies to streamed requests.
    req = flow.request
    if req.pretty_host != "api.anthropic.com" or not req.path.startswith("/v1/messages"):
        return
    with open(_key_file) as f:
        key = f.read().strip()
    req.scheme = _url.scheme
    req.host = _url.hostname
    req.port = _url.port or (443 if _url.scheme == "https" else 80)
    req.headers["Host"] = _url.netloc
    req.headers.pop("authorization", None)
    req.headers["x-api-key"] = key
    if _pin:
        req.headers["x-tokenpool-pin"] = _pin
