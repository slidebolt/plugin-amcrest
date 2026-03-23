# plugin-amcrest Instructions

`plugin-amcrest` follows the reference runnable-module architecture.

- Keep `cmd/plugin-amcrest/main.go` as a thin wrapper only.
- Put runtime lifecycle and device wiring in `app/`.
- Keep protocol/private helpers under `internal/...`.
- Prefer testing `app/` and `internal/...`; keep `cmd` focused on the BDD harness only.
