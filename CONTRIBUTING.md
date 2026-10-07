# Contributing

- Keep comments to 3 lines max, normally one line and ideally inline (`// ...` on the
  code line). Comments explain *why*, never *what*; delete redundant ones.
- `gen/` is committed; regenerate it with `make proto` after editing `proto/`.
- Run `make test` (race detector on) before opening a pull request.
- The usage events table is append-only; migrations live in
  `internal/database/migrations` and are embedded into the binary.
