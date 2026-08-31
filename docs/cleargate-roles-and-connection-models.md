# Machine Cleargate — Roles and Connection Models

*How accounts, people, and agents are structured on the platform, and the three ways a business can connect.*

**Companion to Whitepaper v1.0 — August 2026**

---

## Why this document exists

Early thinking about the platform assumed a simple split: there are *customers* who connect agents and spend, and there are *merchants* who receive payment. In the machine economy this split breaks down. The same business can buy through its agents and sell through its services — sometimes within minutes of each other. A "merchant" is not a distinct kind of user; it is simply an account that happens to be receiving a payment.

This document defines the entities the platform actually needs, separates them from the *roles* those entities play, and describes the three ways a business connects: as a buyer, as a seller, or as both.

The governing principle throughout: **separate what something *is* from what it *does*.** An account is what a business *is* on the platform. Paying and receiving are things it *does*. Modeling it this way means the buyer case, the seller case, and the both case are the same machinery with different participants — no separate account types, no migrations, no special handling.

---

## Part 1 — The entities (what things *are*)

These are the nouns with their own identity and lifecycle. Illustrated with a running example: **DataCorp**, a company that runs agents.

### Principal — who is legally responsible

The real-world person or legal entity that answers for everything: DataCorp Ltd, the registered company. If an agent misbehaves, if there is a legal dispute, or if identity verification is required for compliance, the principal is the accountable party.

The principal is a *legal fact*, not a login. Day-to-day interaction with it is rare, but it is who ultimately owns the money and carries liability. When know-your-customer checks are eventually required, it is the principal who is verified.

*Kept separate because:* one legal entity may own several accounts, and "who is responsible" is a different question from "who is logging in."

### Account — the container that holds money and agents

The entity with a balance. DataCorp's account holds its funds and everything attached to it: agents, registered services, transaction history, billing. When money moves, it moves in or out of an account.

An account is neither "buyer" nor "seller" by nature. Those are roles it takes on per transaction.

*Kept separate because:* one principal may run multiple accounts — production and testing, or one per region — each an independent container.

### User — a human who logs in

An actual person with credentials. At DataCorp: Anna (founder), Ben (engineer), Carol (accountant). They log into the account and act, but not with equal power — see permission roles below.

A user is about *access*: who can log in and what they may do.

*Kept separate because:* the principal is *responsibility* (DataCorp is on the hook) while users are *access* (three people at three permission levels). One company, one principal, many users.

### Agent — the autonomous actor that transacts

The software actor connected to an account, such as DataCorp's `analysis-bot`. It holds a mandate and is the entity that actually initiates payments. Not a human, not a login — the automated actor doing the buying.

### Mandate — the rulebook for one agent

The signed set of rules governing one agent: maximum per purchase, monthly cap, permitted categories, approval threshold. Attached to the agent, signed by a user with authority, and versioned — every historical version is retained so the platform can always answer "what were the rules at the exact moment this payment happened."

### Counterparty — whoever an agent transacts with

Anyone on the receiving end of a payment. This may be an external endpoint or another account on the platform. Reputation attaches here: the platform tracks whether a counterparty delivers, independent of who they are. A counterparty record may exist without an account (an external endpoint) and later be *linked* to an account when that party signs up — at which point they inherit the reputation already accumulated.

### The entities in one sentence

> A **principal** (DataCorp, legally responsible) owns an **account** (the container with the balance), which several **users** (Anna, Ben, Carol) can log into with different permissions, and which has **agents** (`analysis-bot`) connected to it, each governed by a **mandate** (its spending rules), that transact with **counterparties** (whoever they pay or are paid by).

---

## Part 2 — The roles (what things *do*)

Roles are not new entities. They describe how an entity participates in a given moment. The same account, the same user, takes different roles at different times.

### Transaction roles — assumed per payment

**Payer** — the account whose agent is spending. Every payment has exactly one payer.

**Payee** — the party receiving the payment and expected to deliver. This is what earlier thinking called "the merchant." Reputation attaches to this role. A payee may be an on-platform account or an external endpoint — the role is identical either way.

**Approver** — a user who signs off when a payment escalates past a threshold.

**Backer** *(later)* — whoever underwrites guarantees for a class of counterparties.

The key consequence: **"merchant" dissolves into "an account acting in the payee role."** Any account can occupy it. This is what makes agent-to-agent payment the same code path as agent-to-merchant payment — the payee simply happens to be another account rather than an outside business.

### Permission roles — who may do what within an account

Separate from transaction roles, these govern what each user is allowed to do inside their account:

| Permission role | Can do |
|---|---|
| **Owner** | Everything, including billing and closing the account |
| **Admin** | Manage agents and mandates; not billing |
| **Approver** | Approve escalated payments; not change mandates |
| **Viewer** | Read-only: console and reports (e.g. an accountant or auditor) |

This permission model is also the platform's segregation-of-duties mechanism for enterprise: the user who sets a policy need not be the user who approves an exception. It is built early because retrofitting authorization into a running system is notoriously painful.

---

## Part 3 — The three connection models

After creating an account, a business is not forced down a single path. The defining question is *what do you want to do*, and there are three answers. An account can start with one and add the other at any time, with no migration.

An account has two capabilities it can attach:

- **Connect agents** → so it can *buy* (payer side). Requires funding and mandates.
- **Register services** → so it can *sell* (payee side). Requires a payout destination and delivery confirmation.

The three models are simply which capabilities an account attaches.

### Model 1 — Buyer only

**Who:** a company running agents that purchase APIs, data, compute, or other agents' services. The original mental model of the platform.

**Setup:**
1. Create an account (principal, account, first user).
2. Fund it by card, bank transfer, or stablecoin.
3. Connect an agent and set its mandate (caps, categories, approval threshold).
4. Copy the agent credential into the agent's configuration.

**In operation:** the agent transacts; the account acts as **payer**. No services registered, no payout destination needed.

### Model 2 — Seller only

**Who:** a headless merchant, an API, or any business that only wants to *receive* payments from agents. It runs no buying agents of its own.

**Setup:**
1. Create an account (principal, account, first user). Identical signup.
2. Register a service instead of connecting an agent: endpoint URL, what is sold, price, delivery-confirmation method.
3. Set a payout destination — bank account (via off-ramp partner) or stablecoin address.
4. Add one delivery-confirmation call to the server, so successful delivery notifies the platform.

**In operation:** other accounts' agents pay this account; it acts as **payee**, delivers, is paid out (daily), and accumulates payee reputation. No agents, no mandates, no funding — the mirror image of the buyer setup.

### Model 3 — Both

**Who:** the characteristic machine-economy business. It buys through its agents *and* sells through its services. Example: DataCorp, whose `analysis-bot` buys weather data while its `/analyze` service is bought by other agents.

**Setup:** both of the above, in either order, on one account:
- Agents connected, funded, with mandates (to buy).
- Service(s) registered with a payout destination and delivery confirmation (to sell).

**In operation:** the account is **payer** when its agents spend and **payee** when its services are bought. One account, one balance, both flows. Reputation as a seller accrues on the payee side. No second account, no "merchant mode," no special handling — the role is decided per transaction.

---

## Part 4 — Why one account handles both sides

A worked example, following DataCorp through two consecutive transactions.

**Transaction 1 — DataCorp buys weather data.**
DataCorp's `analysis-bot` pays WeatherAPI (an external endpoint). DataCorp's account is the **payer**; its agent's mandate is checked; WeatherAPI is the **payee** and earns reputation.
Ledger: DataCorp −$0.40, WeatherAPI payable +$0.40.

**Transaction 2 — DataCorp sells analysis.**
Minutes later, ClientCorp's agent pays DataCorp for analysis. Now ClientCorp is the **payer** and DataCorp's account is the **payee** — both are on-platform accounts. ClientCorp's mandate is checked; DataCorp's payee reputation is consulted; DataCorp delivers.
Ledger: ClientCorp −$5.00, DataCorp payable +$5.00.

**Net position:** DataCorp's single account shows −$0.40 spent and +$5.00 earned. The identical authorization machinery ran in both transactions. The only difference was which side DataCorp sat on.

Had "merchant" been modeled as a separate entity type, DataCorp would have needed two linked identities — a customer account and a merchant account — and the platform would be stitching them together. Because "payee" is a role any account can occupy, DataCorp is one account that happens to have both agents and services attached.

---

## Summary

**Entities** (identity and lifecycle): Principal, Account, User, Agent, Mandate, Counterparty.

**Transaction roles** (per payment): payer, payee, approver, and later backer.

**Permission roles** (within an account): owner, admin, approver, viewer.

**Connection models:** buyer only (agents attached), seller only (services attached), or both (both attached to one account).

**The three modeling decisions that prevent a future rebuild:**

1. **Merchant is a role (payee), not an entity.** Any account can receive payment, which makes agent-to-agent identical to agent-to-merchant.
2. **Counterparty is tracked separately from Account and may be linked to one.** External endpoints earn reputation without accounts and claim it on signup. Reputation lives on the counterparty, not the account.
3. **Principal (responsibility), User (access), and Account (container) are distinct.** Redundant at three users, essential at three thousand — and the attachment points for future compliance and enterprise features.
