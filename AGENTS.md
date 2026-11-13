# Cleargate Core

Go modular monolith for Cleargate Finance: the buyer-side authorization layer for autonomous-agent payments (USDC, on Solana first, then Base). A human funds an account and signs a **mandate**; an agent then pays within it. Agents hold spending authority, never money. Every payment is checked against the mandate, then ledger debit + spend counter + decision record commit in one transaction, inside a 100ms budget.

## Read before you build

- [dev-docs/mvp-implementation-plan.md](dev-docs/mvp-implementation-plan.md): the build plan, long. Grep for `### Task N.N` and read that task plus its "Why this and not that" boxes; do not read it whole. Its Revision notes record deliberate changes.
- [dev-docs/implementation-plan-v2.md](dev-docs/implementation-plan-v2.md): what comes after the MVP (Phases 9–15: hosting, the console, the sandbox, AWS staging, testers). It continues V1's task numbering, so `### Task N.N` is unique across both files. New work starts here; V1 stays the record and the Why boxes for Phases 0–8.
- [docs/cleargate-finance-whitepaper.md](docs/cleargate-finance-whitepaper.md): the spec. Code comments cite it as `§8.3`; look the section up before changing that code.
- [docs/data-architecture.md](docs/data-architecture.md): read before any migration or schema change.
- [docs/cleargate-roles-and-connection-models.md](docs/cleargate-roles-and-connection-models.md): read before touching entity or role vocabulary.
- [api/openapi.yaml](api/openapi.yaml): the public API contract. The SDK and MCP server are separate repos that generate from it; the console lives here in `web/console/` and generates its client from it too. A new route or a changed response must change it too: `internal/app/openapi_routes_test.go` fails on any route missing from it and on any response that does not match it.
- `docs/research/` holds dated evaluations, not decisions. The Turnkey/Privy note is superseded: the wallet provider is Coinbase CDP behind a `WalletProvider` interface.

## Status and chain

- Build status lives in the plan: a ticked `- [x]` step is built and tested, an open `- [ ]` is not. A package of tiny files marked `TODO(scaffold)` is unbuilt. Check the ticks and `git log` before assuming a feature exists.
- The plan and whitepaper still describe Base (EIP-3009 transfers, CDP wallets, `batch-settlement`). The launch order is now **Solana first, then Base**. Keep everything chain-specific behind the settlement `Rail` and the `WalletProvider` interfaces, and do not assume Base in a module outside them.
- CDP and the x402 Go SDK support Solana. MVP account wallets are CDP Server Wallets. Open decisions in the plan's Revision notes (the primary control model on mainnet, `batch-settlement` support) are the owner's to settle; stop and ask rather than choose.

## Architecture

- Module-first: `internal/modules/<feature>/` with `doc.go`, `domain.go`, `ports.go`, `service.go`, `repo_postgres.go`, a handler file, `module.go`. Read `ledger` or `account` for the shape.
- `make lint` enforces the dependency rules (depguard, `.golangci.yml`): `domain.go` and `service.go` never import gin, pgx or `database/sql`; modules never import `internal/app`.
- A module declares the interfaces it consumes in its own `ports.go`. Cross-module calls go through those interfaces, never through another module's repository or handler.
- Concrete wiring lives only in `internal/app` (`BuildAPI`, `BuildWorker`). `Module.New(Deps)` refuses a missing Mailer or Sealer instead of defaulting to an unsafe one; keep that for security-critical dependencies.
- Two binaries from the same modules: `cmd/api` (hot path) and `cmd/worker` (anything slow). Migrations run via `cmd/migrate` as a deploy step, never on API boot.
- Two caller surfaces: `agentAPI` (API key) and `consoleAPI` (human session). They share no credential, token or route; mandate-mutating routes exist only under `consoleAPI`.
- The console UI is `web/console/` (TypeScript, framework not chosen yet; not built). It is a pure client of `consoleAPI` with no privileges: its only link to Go is the client generated from `api/openapi.yaml`. It never imports Go code, reads the database, or gets a console-only route (a route it needs goes into the public contract). It holds no rules: it never decides what a mandate permits, computes a balance or limit, or builds the bytes to be signed (the server canonicalises, the console passes them through unchanged). The rules stay in `internal/modules`.
- Serve the console and the API from one origin behind a reverse proxy, so no CORS is needed and the session cookie stays first-party. Add a CORS allowlist only for an origin we do not serve.
- Errors: modules return `apperr` values (sentinels like `ErrAgentNotFound = apperr.NotFound(...)`) and never import `net/http`. Handlers call `c.Error(err)` and return; `server.ErrorMapper` is the single place that maps `apperr.Kind` to a status.
- Transactions: `database.UnitOfWork.Do(ctx, fn)` carries the transaction in `ctx`. Repositories join it implicitly and nested calls join the outer one, so repository signatures never mention a tx. Inside `fn`, use the `ctx` it receives, since the outer one runs outside the transaction. Isolation is READ COMMITTED, so concurrency safety comes from single-statement atomic writes or explicit row locks taken in id order, never from read-then-write in Go.
- Time comes from the injected `Clock` in `ports.go`, so tests control "now" without sleeping.

## Invariants that hold everywhere

- Money is `currency.Amount`, an exact decimal bound to an asset. `float64` appears nowhere near money; decode JSON amounts as strings.
- The unit of account is `currency.USD`: ledger, counters, mandates and decisions are all in it. Every USD amount is backed one for one by USDC, the only token, and the rail converts at par at its edge; no other module converts.
- A balance is a claim on the USDC held, not on dollars. If USDC trades away from $1, its market value is shown beside the balance; the ledger never revalues and the platform never covers the difference.
- The ledger is append-only. A correction is a reversing entry, and every entry's postings sum to zero, enforced by a database trigger as well as Go.
- Customer funds never go below zero. `ledger` locks the debited `customer_funds` buckets, then checks the balance, and returns `ErrInsufficientFunds` without writing. Platform buckets are exempt: the on-chain float is negative by design.
- A *ledger account* (accounting bucket) is not an *account* (customer). `internal/shared/id` keeps them as distinct types; use typed IDs and never pass bare strings.
- Agents hold spend counters, not balances. Money is pooled per account.
- Mandates are immutable and versioned: a change inserts a new version. The signed bytes are stored opaque and never re-marshalled.
- `authz.Decide` is pure: no `ctx`, no `error`, no I/O. Fetch every input before calling it.
- Decision records are immutable and hash-chained, with the feature snapshot captured at decision time; it cannot be reconstructed later.
- Enforce rules in the database (CHECK, partial unique indexes, triggers) so no future code path can bypass them, and validate in Go for a good error message.
- Store digests of tokens, sessions and API keys, never the token. A secret the server must read back (TOTP) goes through `crypto` envelope encryption. Agent credentials authenticate only; they carry no authority.
- No code references a reserve, a coverage limit or absorbing a loss. Guarantees are out of scope; holds withhold money already held.
- Account roles are `owner`, `admin`, `guardian`, `viewer`.

## Conventions

- Comments explain *why*: the decision, the trade-off, the whitepaper section. Match the density of the neighbouring file.
- Tests: no build tag means a fast unit test with no external dependencies (`make test`). Anything needing Postgres or Redis carries `//go:build integration`, uses `test/fixtures` (real testcontainers; no mocks of the database), and runs under `make test-int` with Docker. Name tests `TestSubject_Behaviour`; use testify.
- Migrations are numbered pairs in `migrations/` (`NNNNNN_name.up.sql` and `.down.sql`). Write the down file too.
- Config is environment-only; add new variables to `.env.example` with a comment. `DATABASE_DSN` and `CRYPTO_MASTER_KEYS` have no default.
- Conventional Commits (`feat(account): ...`), work on `feature/*` branches, merged by PR.
- `make help` lists the commands. A change is done when `make check` is green (gofmt, vet, lint, unit and integration tests, about 30s; needs Docker).
