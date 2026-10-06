# Contributing

Thanks for helping. Bug reports, fixes and new upstream formats are all
welcome.

## Before you start

For anything bigger than a small fix, open an issue first so we can agree
on the approach. tokenpool aims to stay one binary with one YAML file and
no database.

## Working on the code

You need Go 1.23 or newer.

```sh
go build ./...
go vet ./...
go test -race ./...
gofmt -l .        # prints nothing when formatting is clean
```

- `internal/config`: YAML loading, `${ENV_VAR}` expansion, validation,
  model mapping
- `internal/pool`: upstream order, cooldowns, the admin-managed pool file
- `internal/translate`: Anthropic ⇄ OpenAI requests, responses and streams
- `internal/proxy`: HTTP server, failover, rate-limit parsing, admin API
- `cmd/tokenpool`: the binary

New behavior needs a test. Proxy changes are easiest to test end to end
with the fake upstreams in `internal/proxy/proxy_test.go`.

## Pull requests

- Keep each pull request to one change.
- Commit titles: imperative, at most 50 characters. Wrap bodies at 72.
- Never paste real API keys into code, tests, issues or logs. Use
  obviously fake values such as `sk-ant-test`.
