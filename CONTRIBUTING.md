# Contributing

Thanks for considering a contribution.

## Development

```bash
go test ./...
go vet ./...
gofmt -w *.go
```

## Rules

- Do not commit secrets or local `config.yaml` files.
- Keep provider credentials behind environment variables.
- Add tests for behavior changes.
- Keep `code_research` tools read-only and bounded.

## Commit style

Use Conventional Commits where practical:

```text
feat: add feature
fix: fix bug
docs: update docs
test: add tests
chore: tooling/config
```
