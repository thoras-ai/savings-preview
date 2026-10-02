# Thoras Savings Preview

A read-only CLI that reads a cluster's existing usage history, writes it as one OTel metrics file, and shows what Thoras would have saved.

- `docs/contract.md`: the OTel file every adapter writes and the report reads.
- `fixtures/savings-preview.otlp.json`: a synthetic file that follows the contract. Regenerate it with `python3 scripts/gen_fixture.py`.
- `scripts/validate_contract.py FILE`: checks a file against the contract.
- `docs/prometheus-adapter.md`: the Prometheus adapter. Build it with `go build -o bin/savings-preview ./cmd/savings-preview`.

Run the Go tests with `go test ./...` and the contract tests with `python3 -m unittest discover -s tests`. The scripts need Python 3 and the standard library only.
