---
name: add-migration
description: Migration — adding or changing a table, column, constraint, index or trigger under migrations/. Read before writing any SQL.
---

# Add a migration

1. Read `docs/data-architecture.md` §4 (the six schema principles) and the §5 section for the area you are changing. Read the newest migration in `migrations/` for style: its comments carry the reasoning, so yours should too.
2. Create the next numbered pair, `NNNNNN_name.up.sql` and `.down.sql`. The down file reverses the up file exactly, dropping tables before the functions their triggers use.
3. Put every rule the system relies on into the database (CHECK, partial unique index, trigger, foreign key). A rule that lives only in Go is bypassed by the first backfill or hotfix.
4. For each such rule, write an integration test that commits the violation and expects the database to refuse it. `internal/modules/payment/schema_test.go` is the model.
5. Prove the round trip on a scratch database: `make dev-up`, set `DATABASE_DSN` from `.env.example` and `CRYPTO_MASTER_KEYS=1:$(openssl rand -base64 32)`, then `make migrate-up`, `make migrate-down`, `make migrate-up`. After the down step `make migrate-version` is one lower and the new objects are gone.
6. A column that holds free text or personal data needs an erasure path before it ships; see the Task 5.4 warning in the plan and `docs/data-architecture.md` §8.

Done when the round trip passes, each rule has a refusing test, and `make check` is green.
