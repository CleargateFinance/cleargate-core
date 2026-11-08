# Cleargate: hackathon submission

**Encode Club Solana hackathon, 2026. Track: Best Use of x402 on Solana.**

Cleargate lets AI agents pay for APIs, data and services over x402 on Solana without ever holding money. A person funds an account in USDC and sets a mandate for each agent: caps per payment, per day and per month, the categories it may buy, and optionally the payees it may pay. When a merchant answers `402`, the agent asks Cleargate to pay. Cleargate checks the mandate, records the decision, and returns a signed Solana USDC transfer for the agent to hand to the merchant. The merchant's facilitator submits it, and the merchant delivers. A compromised agent can spend no more than its mandate allows, and every decision, declines included, is on the record.

The technical detail is in the [README](README.md). This page maps it to the brief.

## Demo video

> **To add before submitting:** a link to the 2–4 minute video.

## The complete flow

The brief asks for a payment request, verification or settlement on Solana, and delivery of the resource:

1. **Payment request.** The agent calls a paid endpoint. The merchant answers `402` with an x402 v2 `PAYMENT-REQUIRED` offering `exact` payment in USDC on Solana devnet.
2. **Authorization.** The agent sends that requirement to `POST /v1/payments/authorize`. Cleargate reads the amount and payee from it and checks the agent's mandate. In one database transaction it then records the payment, debits the account's ledger, counts the spend and records the decision.
3. **Payment.** On approval, Cleargate builds a USDC transfer with the x402 Go SDK, and the account's Coinbase CDP wallet signs it. The agent sends it to the merchant as `PAYMENT-SIGNATURE`.
4. **Verification and settlement on Solana.** The merchant's facilitator verifies the transfer, adds its signature as fee payer and submits it. Cleargate's worker then finds the transaction in the wallet's history at `finalized` and marks the payment `settled`.
5. **Delivery.** The merchant returns the resource.

The README has [a sequence diagram of the flow](README.md#how-a-payment-works). The [demo](demo/README.md) runs the whole flow on devnet, against a real x402 merchant and the public x402.org facilitator. In a run on 8 November 2026, [this transaction](https://explorer.solana.com/tx/3av7JQG5ZCZ1y9FersMXhyy1XKMrxcxfZ9WxF2oNcDT5mA4Yugaih4JeSB14enZMZuWAg9B4tyn58vYnWEqtR3Ts?cluster=devnet) paid for a quote. The facilitator was the fee payer, the account's wallet signed only as the owner of the USDC, and the memo was the payment's ID. The worker marked the payment `settled` 30 seconds after it landed.

## How Solana powers Cleargate

- **USDC on Solana is the money.** Every account balance is backed one for one by USDC in the account's own wallet on Solana. The ledger records what each USDC is for, and the chain is where it moves.
- **A transaction names its fee payer separately from the token owner.** The account wallet signs only as the owner of the USDC, and the merchant's facilitator pays the fee and submits. Customers' wallets never need SOL, so owners never have to deal with gas.
- **Fees small enough for paying per request.** A payment of one cent for one API call costs a fraction of a cent to settle, so paying per request is viable instead of a subscription.
- **Blockhash expiry gives a definite moment after which a payment can never land.** A Solana transaction can only land before its blockhash's last valid block height. Cleargate waits past that height, plus a margin, then returns the funds and the agent's budget with certainty, rather than guessing from a timeout. That is what makes it safe to give back a payment that was signed but never submitted.
- **Fast finality.** Transactions reach `finalized` in seconds. The worker polls every 30 seconds, so deposits are credited and payments confirmed within about a minute, and nothing is concluded from a block that could still change.
- **Memos make payments traceable.** Each transfer carries the payment's ID as its memo, so the worker can find it in the wallet's history. The signed message bytes then prove which payment it is.
- **CDP Server Wallets on Solana.** Coinbase holds the keys and signs on request. No private key exists anywhere in Cleargate.

There is more detail in [Settlement on Solana](README.md#settlement-on-solana).

## Why it is more than a basic integration

- **It is the buyer's side of x402.** Most x402 projects are a paywall for a seller, or a client with a funded hot wallet. Cleargate is the control layer between the two, and it works with any x402 merchant on Solana with no change on the merchant's side.
- **Agents hold spending authority, never money or keys.** An agent's API key only proves which agent is asking. What it may spend comes from a mandate that only a person can change, from a separate console surface with a second factor.
- **One pooled balance, many agents.** Each agent has its own budget against the account's funds.
- **A real double-entry ledger.** It is append-only, every entry sums to zero, and customer funds never go below zero. All three are enforced in Postgres as well as in Go, and reconciled by scheduled jobs.
- **Every decision is recorded**, declines included, with a snapshot of the inputs it was made on, so an owner can see what their agents tried to do as well as what they did.
- **Built like a payment system.** It has idempotency keys, row locks taken in a fixed order, and integration tests against real Postgres. The [OpenAPI contract](api/openapi.yaml) is checked against the server by tests.

## Failed payments

Failure handling is designed in, not added afterwards:

- **A decline moves nothing.** It returns a `reason_code` telling the agent what to do, such as waiting for the daily window or stopping and asking a person, and it is recorded in the decision log. The demo shows three: over the per-payment cap, a payee not on the allowlist, and over the daily cap.
- **A retry is free.** The same idempotency key returns the same answer and the same transaction, and charges once.
- **A signed payment that never lands gives everything back.** Once its blockhash has expired, the worker marks it `expired`, posts a reversing ledger entry and returns the spend to the budget windows it was charged to. A transaction that fails on chain is handled the same way.
- **A Solana node outage releases nothing early.** An incomplete read concludes nothing, because releasing funds for a payment that then lands is the one mistake that loses money.
- **The agent fails closed.** A timeout or a `5xx` from Cleargate means *not paid*, and the agent must not pay by another route.

The full table is in [When a payment fails](README.md#when-a-payment-fails).

## The judging criteria

| Criterion | Where to look |
|---|---|
| Meaningful integration of x402 and Solana | [How a payment works](README.md#how-a-payment-works) and [Settlement on Solana](README.md#settlement-on-solana). In the code: `internal/infrastructure/solana/x402.go`, `internal/modules/settlement/` |
| Reliability of the payment-to-access flow | Idempotency, finality and expiry rules in the README. In the tests: `internal/app/settle_test.go` and `TestMVP_DefinitionOfDone`. On devnet: the [demo](demo/README.md) |
| A compelling use case | [Why](README.md#why) in the README, and the [whitepaper](docs/cleargate-finance-whitepaper.md) |
| Clear documentation, and sensible handling of failed payments | [When a payment fails](README.md#when-a-payment-fails), [the API](README.md#api), [`api/openapi.yaml`](api/openapi.yaml) |

| Requirement | Where |
|---|---|
| A working demo on Solana devnet | [demo/README.md](demo/README.md), summarised in the README's [Demo](README.md#demo) |
| A repository with a README and setup instructions | [Running it locally](README.md#running-it-locally) |
| A 2–4 minute demo video | [Above](#demo-video) *(to add)* |
| How Solana powers the project | [Above](#how-solana-powers-cleargate) |

## Built with

Go, Gin, PostgreSQL, Redis, the [x402 Go SDK](https://github.com/x402-foundation/x402) (its SVM `exact` client), [solana-go](https://github.com/gagliardetto/solana-go), and the [Coinbase CDP](https://docs.cdp.coinbase.com) Go SDK for Server Wallets.

This is a testnet MVP. What it does not do yet is listed in the README's [Known gaps](README.md#known-gaps).
