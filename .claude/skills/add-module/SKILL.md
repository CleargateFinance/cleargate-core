---
name: add-module
description: Module — creating a package under internal/modules/ or filling a TODO(scaffold) one. Read before the first file.
---

# Add a module

1. Run `ls internal/modules`. A package of tiny files marked `TODO(scaffold)` is the place to build; do not create a second one beside it.
2. Copy the shape of `internal/modules/ledger` (service and repository) and `internal/modules/account` (handlers): `doc.go`, `domain.go`, `ports.go`, `service.go`, `repo_postgres.go`, a handler file, `module.go`.
3. `doc.go` states the invariants the module holds and cites the whitepaper sections they come from. Comments everywhere explain why, at the density of the neighbouring file.
4. Declare the interfaces the module consumes in its own `ports.go`, and call other modules only through them. `domain.go` and `service.go` import no gin, pgx or `database/sql`; `module.go` imports nothing from `internal/app`.
5. Return `apperr` values from the service. `Module.New(Deps)` refuses a missing security-critical dependency (Mailer, Sealer, signer) instead of defaulting.
6. Wire the module in `internal/app` (`BuildAPI`, `BuildWorker`) and nowhere else.
7. Write a unit test for the service with no build tag, and an integration test for the repository with `//go:build integration` and `test/fixtures`.

Done when the module is wired, every file has a test that fails if the file's behaviour breaks, and `make check` is green (`make lint` enforces the import rules).
