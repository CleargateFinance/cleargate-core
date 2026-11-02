-- Payments and the decisions that judged them.
--
-- Whitepaper §5.4, §8.4, §12.1; data-architecture.md §4.3, §5.2, §5.3, §6.
--
-- payment and decision are 1:1, and both exist even when no money moves. A
-- declined payment is a full row with a decision behind it, so the console's
-- "blocked 14 payments, $3,200 at risk" is a query rather than a log scrape.
--
-- Four tables:
--   payment          one attempt by an agent to pay, approved or not
--   decision         the engine's judgement of it, with every input it saw
--   decision_check   one row per pipeline stage that ran
--   idempotency_key  the remembered outcome of a request, for safe retries
-- and event_outbox, written in the same transaction as the state it reports.
--
-- Not here yet, each arriving with the code that writes it: counterparty
-- (resolved from pay_to and resource_url by Task 5.3) and fulfillment
-- (delivery reporting, Task 5.5).

-- ---------------------------------------------------------------------------
-- Keys other tables check their consistency against
-- ---------------------------------------------------------------------------

-- Lets a payment name its agent and account together, so the two cannot
-- disagree about whose agent spent.
ALTER TABLE agent ADD CONSTRAINT agent_id_account_unique UNIQUE (id, account_id);

-- Lets a decision name the exact mandate version and bytes it was checked
-- against, and have the database confirm that mandate exists with exactly
-- that version and digest, for that agent.
ALTER TABLE mandate ADD CONSTRAINT mandate_evidence_unique
    UNIQUE (id, agent_id, version, payload_digest);

-- ---------------------------------------------------------------------------
-- Payments
-- ---------------------------------------------------------------------------

CREATE TABLE payment (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id  uuid NOT NULL,
    agent_id    uuid NOT NULL,

    -- As requested. Zero or negative only on a decline: that is the
    -- invalid_amount decline, and it is recorded like any other.
    amount      numeric(38, 18) NOT NULL,
    asset       text NOT NULL,

    -- As the agent sent them, bounded so a decision record stays small. Null
    -- when not sent; a payment can be declined before a payee is known.
    category     text CHECK (char_length(category) <= 256),
    pay_to       text CHECK (char_length(pay_to) <= 256),
    resource_url text CHECK (char_length(resource_url) <= 2048),

    -- How the payment moves, for reporting and reconciliation only. No rule
    -- reads these and no signature covers them, so adding a rail never
    -- changes policy (Task 5.1). The ledger counts USD; only these say what
    -- would move on-chain. network is a CAIP-2 chain id.
    rail        text NOT NULL CHECK (rail IN ('x402-exact-solana')),
    token       text NOT NULL CHECK (token IN ('USDC')),
    network     text NOT NULL CHECK (network ~ '^[-a-z0-9]{3,8}:[-_a-zA-Z0-9]{1,32}$'),

    -- declined is final at once. authorized waits for the chain (Task 7.7)
    -- and ends settled, expired or failed.
    status      text NOT NULL CHECK (status IN ('declined', 'authorized', 'settled', 'expired', 'failed')),

    -- The decision instant, shared with the decision row (see below).
    decided_at  timestamptz NOT NULL,

    -- When the payment reached a final status. Null exactly while authorized.
    resolved_at timestamptz,

    created_at  timestamptz NOT NULL DEFAULT now(),

    FOREIGN KEY (agent_id, account_id) REFERENCES agent (id, account_id),

    CHECK (status = 'declined' OR amount > 0),
    CHECK ((status = 'authorized') = (resolved_at IS NULL)),
    CHECK (resolved_at IS NULL OR resolved_at >= decided_at),

    -- Targets for the composite foreign keys below.
    UNIQUE (id, account_id),
    UNIQUE (id, account_id, agent_id, decided_at)
);

-- The console's history per agent and per account, newest first.
CREATE INDEX payment_by_agent   ON payment (agent_id, created_at DESC);
CREATE INDEX payment_by_account ON payment (account_id, created_at DESC);

-- What Task 7.7's worker sweeps: payments waiting on the chain.
CREATE INDEX payment_awaiting_settlement ON payment (decided_at) WHERE status = 'authorized';

-- A payment's request is a fact and never changes. Its status moves forward
-- once, from authorized to an outcome, and nothing else ever moves:
--
--   (insert) -> declined
--   (insert) -> authorized -> settled | expired | failed
--
-- Settling and expiring also write ledger entries (Task 7.7). This row only
-- records which outcome happened.
CREATE OR REPLACE FUNCTION payment_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'payment is append-only, DELETE is not permitted';
    END IF;

    IF TG_OP = 'INSERT' THEN
        IF NEW.status NOT IN ('declined', 'authorized') THEN
            RAISE EXCEPTION 'a payment starts declined or authorized, not %', NEW.status;
        END IF;
        RETURN NEW;
    END IF;

    IF (NEW.id, NEW.account_id, NEW.agent_id, NEW.amount, NEW.asset, NEW.category,
        NEW.pay_to, NEW.resource_url, NEW.rail, NEW.token, NEW.network,
        NEW.decided_at, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.account_id, OLD.agent_id, OLD.amount, OLD.asset, OLD.category,
        OLD.pay_to, OLD.resource_url, OLD.rail, OLD.token, OLD.network,
        OLD.decided_at, OLD.created_at)
    THEN
        RAISE EXCEPTION 'a payment''s request is immutable, only its status moves';
    END IF;

    IF NOT (OLD.status = 'authorized' AND NEW.status IN ('settled', 'expired', 'failed')) THEN
        RAISE EXCEPTION 'payment cannot move from % to %', OLD.status, NEW.status;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER payment_guard
    BEFORE INSERT OR UPDATE OR DELETE ON payment
    FOR EACH ROW EXECUTE FUNCTION payment_guard();

-- ---------------------------------------------------------------------------
-- Decisions
-- ---------------------------------------------------------------------------

-- Partitioned by month from day one, like the ledger, because this table
-- grows with every request including the declines (data-architecture.md §4.5).
CREATE TABLE decision (
    id           uuid NOT NULL DEFAULT gen_random_uuid(),
    payment_id   uuid NOT NULL,
    account_id   uuid NOT NULL,
    agent_id     uuid NOT NULL,

    outcome      text NOT NULL CHECK (outcome IN ('approved', 'declined')),

    -- The public contract of Task 4.1. A new code is a migration, which is
    -- the point: it is also an API change.
    reason_code  text NOT NULL CHECK (reason_code IN (
                     'ok',
                     'invalid_amount', 'account_blocked', 'no_mandate',
                     'mandate_expired', 'asset_not_permitted',
                     'category_not_permitted', 'payee_not_permitted',
                     'over_per_tx_cap', 'over_daily_cap', 'over_monthly_cap',
                     'insufficient_funds', 'processing_error'
                 )),
    -- For people. Reworded freely; never aggregated on.
    reason_text  text NOT NULL,

    -- The mandate evaluated, all three or none. Null when the agent had none,
    -- which can decline as no_mandate or as one of the two codes checked
    -- before the mandate.
    mandate_id      uuid,
    mandate_version int,
    mandate_digest  bytea,

    -- Every input the engine saw (Task 4.2), versioned by its "v" key.
    features     jsonb NOT NULL,

    evaluated_at timestamptz NOT NULL,
    latency_ms   int NOT NULL CHECK (latency_ms >= 0),

    -- Tamper evidence, nullable until Task 5.4 starts the chain.
    prev_hash    bytea,
    hash         bytea,
    signature    bytea,

    PRIMARY KEY (id, evaluated_at),

    -- One decision per payment. A partitioned table's unique constraints
    -- must include the partition key, so uniqueness on payment_id alone is
    -- not expressible. Instead the foreign key pins evaluated_at to the
    -- payment's single decided_at, and this makes the pair unique.
    UNIQUE (payment_id, evaluated_at),
    FOREIGN KEY (payment_id, account_id, agent_id, evaluated_at)
        REFERENCES payment (id, account_id, agent_id, decided_at),

    -- Either no mandate, or one that exists with exactly this version and
    -- these bytes, for this agent. The three columns are null together (the
    -- CHECK below), so the key's default matching, which skips the check when
    -- any column is null, skips it only when there is no mandate. MATCH FULL
    -- would not do: agent_id is part of the key and never null.
    FOREIGN KEY (mandate_id, agent_id, mandate_version, mandate_digest)
        REFERENCES mandate (id, agent_id, version, payload_digest),
    CHECK ((mandate_id IS NULL) = (mandate_version IS NULL)
           AND (mandate_id IS NULL) = (mandate_digest IS NULL)),

    CHECK ((outcome = 'approved') = (reason_code = 'ok')),
    CHECK (reason_code <> 'no_mandate' OR mandate_id IS NULL),
    CHECK (mandate_id IS NOT NULL
           OR reason_code IN ('no_mandate', 'invalid_amount', 'account_blocked')),

    -- The columns and the snapshot agree about whether there was a mandate.
    -- Written so a missing key fails: a CHECK that evaluates to NULL passes.
    CHECK (COALESCE(jsonb_typeof(features -> 'v'), '') = 'number'),
    CHECK (features ? 'mandate'
           AND (mandate_id IS NULL) = (features -> 'mandate' = 'null'::jsonb))
) PARTITION BY RANGE (evaluated_at);

-- The console's decision log per agent, newest first (Task 6.2).
CREATE INDEX decision_by_agent ON decision (agent_id, evaluated_at DESC);

CREATE TABLE decision_check (
    decision_id  uuid NOT NULL,
    evaluated_at timestamptz NOT NULL,

    -- Position in the pipeline, from 1. The last check of a decline is the
    -- one that failed.
    ordinal      smallint NOT NULL CHECK (ordinal > 0),
    stage        text NOT NULL CHECK (stage IN (
                     'request', 'account', 'mandate',
                     'counterparty', 'intent', 'behaviour', 'funds'
                 )),
    result       text NOT NULL CHECK (result IN ('pass', 'fail', 'skipped')),

    PRIMARY KEY (decision_id, evaluated_at, ordinal),
    UNIQUE (decision_id, evaluated_at, stage),
    FOREIGN KEY (decision_id, evaluated_at) REFERENCES decision (id, evaluated_at)
) PARTITION BY RANGE (evaluated_at);

-- Evidence is never edited. reject_mutation is the ledger's (000002).
CREATE TRIGGER decision_append_only
    BEFORE UPDATE OR DELETE ON decision
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

CREATE TRIGGER decision_check_append_only
    BEFORE UPDATE OR DELETE ON decision_check
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- Every payment has its decision, and they agree about the outcome. Deferred
-- to commit, since the payment is inserted first and its decision after it.
CREATE OR REPLACE FUNCTION assert_payment_decided() RETURNS trigger AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM decision d
         WHERE d.payment_id = NEW.id
           AND d.evaluated_at = NEW.decided_at
           AND (d.outcome = 'declined') = (NEW.status = 'declined')
    ) THEN
        RAISE EXCEPTION 'payment % has no decision with a matching outcome', NEW.id;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER payment_decided
    AFTER INSERT ON payment
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION assert_payment_decided();

-- Monthly partitions for both decision tables. Separate from the ledger's
-- function so each module maintains its own tables (decisionlog does, from
-- the worker), and idempotent for the same reason.
CREATE OR REPLACE FUNCTION ensure_decision_partitions(
    months_back  int DEFAULT 1,
    months_ahead int DEFAULT 3
) RETURNS void AS $$
DECLARE
    offset_months int;
    range_start   date;
    range_end     date;
    part_name     text;
    parent        text;
BEGIN
    FOR offset_months IN -months_back..months_ahead LOOP
        range_start := date_trunc('month', current_date)::date
                       + (offset_months || ' months')::interval;
        range_end   := range_start + interval '1 month';

        FOREACH parent IN ARRAY ARRAY['decision', 'decision_check'] LOOP
            part_name := parent || '_' || to_char(range_start, 'YYYY"m"MM');

            IF NOT EXISTS (
                SELECT 1 FROM pg_class WHERE relname = part_name
            ) THEN
                EXECUTE format(
                    'CREATE TABLE %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)',
                    part_name, parent, range_start, range_end
                );
            END IF;
        END LOOP;
    END LOOP;
END;
$$ LANGUAGE plpgsql;

SELECT ensure_decision_partitions(1, 12);

-- ---------------------------------------------------------------------------
-- Idempotency
-- ---------------------------------------------------------------------------

-- The durable guard against a retry paying twice (§8.4). Redis may sit in
-- front of it; this primary key is what makes a duplicate impossible with
-- every cache cold.
--
-- Only final outcomes are stored, so a row always names its payment. A
-- request that failed with a server error rolled back and left nothing here,
-- and its retry is evaluated afresh (Task 5.2).
CREATE TABLE idempotency_key (
    account_id   uuid NOT NULL,
    key          text NOT NULL CHECK (char_length(key) BETWEEN 1 AND 255),

    -- sha256 of the canonical request. The same key with a different body is
    -- a client bug, answered with 409 rather than the old result.
    request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),

    payment_id   uuid NOT NULL,
    response     jsonb NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (account_id, key),
    FOREIGN KEY (payment_id, account_id) REFERENCES payment (id, account_id)
);

-- What the retention worker deletes by: keys older than 24 hours (Task 5.2).
CREATE INDEX idempotency_key_by_age ON idempotency_key (created_at);

-- A remembered outcome never changes. Rows are deleted when they expire, so
-- only UPDATE is refused.
CREATE TRIGGER idempotency_key_immutable
    BEFORE UPDATE ON idempotency_key
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- ---------------------------------------------------------------------------
-- Outbox
-- ---------------------------------------------------------------------------

-- Events written in the same transaction as the change they report, so an
-- event exists exactly when its change committed (data-architecture.md §6).
-- The relay that publishes them comes with a second consumer; the table comes
-- now, because events not written today are events never had.
--
-- Payloads carry identifiers only, never personal data (data-architecture.md
-- §8).
CREATE TABLE event_outbox (
    id             bigserial PRIMARY KEY,
    aggregate_type text NOT NULL CHECK (aggregate_type ~ '^[a-z][a-z_]{0,63}$'),
    aggregate_id   uuid NOT NULL,
    event_type     text NOT NULL CHECK (event_type ~ '^[a-z][a-z_.]{0,127}$'),
    payload        jsonb NOT NULL,
    occurred_at    timestamptz NOT NULL DEFAULT now(),
    published_at   timestamptz,

    CHECK (published_at IS NULL OR published_at >= occurred_at)
);

CREATE INDEX outbox_unpublished ON event_outbox (id) WHERE published_at IS NULL;

-- An event is a fact. The relay may mark it published, once, and may delete
-- it only after that.
CREATE OR REPLACE FUNCTION event_outbox_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.published_at IS NULL THEN
            RAISE EXCEPTION 'event % has not been published and cannot be deleted', OLD.id;
        END IF;
        RETURN OLD;
    END IF;

    IF OLD.published_at IS NOT NULL
       OR NEW.published_at IS NULL
       OR (NEW.id, NEW.aggregate_type, NEW.aggregate_id, NEW.event_type, NEW.payload, NEW.occurred_at)
          IS DISTINCT FROM
          (OLD.id, OLD.aggregate_type, OLD.aggregate_id, OLD.event_type, OLD.payload, OLD.occurred_at)
    THEN
        RAISE EXCEPTION 'an event may only be marked published, once';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER event_outbox_guard
    BEFORE UPDATE OR DELETE ON event_outbox
    FOR EACH ROW EXECUTE FUNCTION event_outbox_guard();
