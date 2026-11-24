# Contributing

Thanks for helping. Issues and pull requests are welcome.

## Setup

Go 1.27 and a C compiler (SQLite is linked through cgo). No WhatsApp account is needed for development.

```bash
go test -race -tags sqlite_fts5 .                 # the whole suite on a synthetic mirror
go build -tags sqlite_fts5 -o wago .
WAGO_API_KEY=dev ./wago -dev-fake -home /tmp/wago-dev    # every session is a recording stand-in
python3 conformance/waha_conformance.py --url http://127.0.0.1:3000 --key dev
```

`POST /api/{session}/dev/incoming` plays inbound messages, voice notes, typing and messages from the owner's phone
against the fake server, so a feature can be tried end to end without a phone. `WAGO_TEST_MIRROR=<copy of a real
mirror>` adds the tests that need real-world history.

## Guidelines

- **Keep the WAHA contract.** Paths, fields, ids and the webhook envelope stay compatible; the conformance suite must
  pass. Additions are fine, renames are not.
- **API stability.** Within API version 1, add routes, fields and events, never remove or rename them.
- **Defaults that work, overrides that are possible.** A new behaviour gets a working default and a config field in
  the `agent` or `policy` block, documented in `wago.example.yaml` and the docs.
- **Fix the cause.** Classify errors by the library's error values, not by message text; no special cases for one
  input.
- **Tests with the change.** Agent-layer behaviour is tested on the engine's clock (see `agent_test.go`), not with
  sleeps.
- **Document why.** A design decision with trade-offs goes into [docs/DESIGN.md](docs/DESIGN.md).
- Never commit real phone numbers, keys, session files or mirrors.

## Pull requests

Run `go vet -tags sqlite_fts5 .` and the tests, describe what changes for users, and add a line to
[CHANGELOG.md](CHANGELOG.md).
