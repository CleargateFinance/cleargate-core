-- The double-entry book.
--
-- Three tables:
--   ledger_account  an accounting bucket money can sit in
--   journal_entry   one economic event, for example a single payment
--   posting         one signed line inside an event
--
-- Naming note: ledger_account is an accounting bucket, NOT a customer. The
-- customer-facing account table is a separate thing entirely, and the two are
-- never interchangeable.

-- ---------------------------------------------------------------------------
-- Buckets
-- ---------------------------------------------------------------------------

CREATE TABLE ledger_account (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- NULL for platform-owned buckets such as revenue or on-chain float,
    -- which belong to no single customer.
    account_id uuid,

    type       text NOT NULL CHECK (type IN (
                   'customer_funds',   -- spendable balance held for a customer
                   'payee_payable',    -- owed to a payee, not yet settled
                   'revenue',          -- platform earnings
                   'reserve',          -- capital held back against losses
                   'onchain_float'     -- value actually sitting on a chain
               )),

    asset      text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),

    -- NULLS NOT DISTINCT so platform buckets, which have a NULL account_id,
    -- cannot be duplicated either. Without it Postgres treats every NULL as
    -- unique and would happily create a second revenue bucket.
    CONSTRAINT ledger_account_unique
        UNIQUE NULLS NOT DISTINCT (account_id, type, asset)
);

CREATE INDEX ledger_account_by_account ON ledger_account (account_id);

-- ---------------------------------------------------------------------------
-- Events and their lines
-- ---------------------------------------------------------------------------

CREATE TABLE journal_entry (
    id             uuid NOT NULL DEFAULT gen_random_uuid(),

    kind           text NOT NULL CHECK (kind IN (
                       'payment',
                       'funding',
                       'payout',
                       'refund',
                       'reversal',
                       'correction'
                   )),

    -- What caused this entry, for example a payment id. Kept loose because the
    -- tables it points at are added later, and a hard foreign key here would
    -- force this migration to know about all of them.
    reference_type text,
    reference_id   uuid,

    -- Which agent's activity produced this entry, when one did. This is the
    -- attribution link that lets per-agent spending be recomputed from the
    -- postings themselves rather than trusted blindly.
    agent_id       uuid,

    description    text,
    created_at     timestamptz NOT NULL DEFAULT now(),

    -- created_at is part of the key because a partitioned table must include
    -- its partition key in every unique constraint.
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

CREATE TABLE posting (
    id                uuid NOT NULL DEFAULT gen_random_uuid(),
    journal_entry_id  uuid NOT NULL,
    ledger_account_id uuid NOT NULL REFERENCES ledger_account (id),

    -- Signed: negative debits the bucket, positive credits it. A single signed
    -- column makes the balance rule literally SUM(amount) = 0, which the
    -- trigger below checks directly. A separate debit/credit flag would turn
    -- that into a CASE expression that is slower and easy to get backwards.
    amount            numeric(38, 18) NOT NULL CHECK (amount <> 0),

    asset             text NOT NULL,
    created_at        timestamptz NOT NULL,

    PRIMARY KEY (id, created_at),

    -- Ties every posting to a real entry, and forces the two timestamps to
    -- match. That match is what lets the balance trigger below restrict its
    -- lookup to one partition instead of scanning them all.
    FOREIGN KEY (journal_entry_id, created_at)
        REFERENCES journal_entry (id, created_at)
) PARTITION BY RANGE (created_at);

CREATE INDEX posting_by_account ON posting (ledger_account_id, created_at DESC);
CREATE INDEX posting_by_entry ON posting (journal_entry_id);

-- ---------------------------------------------------------------------------
-- Append-only enforcement
-- ---------------------------------------------------------------------------

-- Corrections are made by writing a new reversing entry, never by editing or
-- removing history. Raising an exception is deliberate: a rule that silently
-- discarded the write would let a buggy script believe it had succeeded.
CREATE OR REPLACE FUNCTION reject_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION
        '% is append-only, % is not permitted. Write a reversing entry instead.',
        TG_TABLE_NAME, TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER journal_entry_append_only
    BEFORE UPDATE OR DELETE ON journal_entry
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

CREATE TRIGGER posting_append_only
    BEFORE UPDATE OR DELETE ON posting
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- ---------------------------------------------------------------------------
-- The balance invariant
-- ---------------------------------------------------------------------------

-- Every entry's postings must sum to exactly zero. Enforcing this in the
-- database rather than only in application code matters because application
-- checks protect only the paths that remember to call them, while a backfill,
-- an admin script or a hotfix can bypass them. This cannot be bypassed.
CREATE OR REPLACE FUNCTION assert_entry_balanced() RETURNS trigger AS $$
DECLARE
    total numeric(38, 18);
BEGIN
    -- created_at is included so Postgres can prune to the single partition
    -- holding this entry. The foreign key above guarantees it matches.
    SELECT COALESCE(SUM(amount), 0)
      INTO total
      FROM posting
     WHERE journal_entry_id = NEW.journal_entry_id
       AND created_at = NEW.created_at;

    IF total <> 0 THEN
        RAISE EXCEPTION
            'journal entry % is unbalanced, its postings sum to %',
            NEW.journal_entry_id, total;
    END IF;

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- DEFERRABLE INITIALLY DEFERRED so the check runs once at COMMIT rather than
-- after each row. Postings are inserted one at a time, so an immediate check
-- would always fail on the first line, before its counter-line exists.
CREATE CONSTRAINT TRIGGER posting_balanced
    AFTER INSERT ON posting
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION assert_entry_balanced();

-- ---------------------------------------------------------------------------
-- Partition management
-- ---------------------------------------------------------------------------

-- Partitioning by month is set up now, while the tables are empty, because
-- adding it later to a table holding hundreds of millions of rows is a
-- migration project rather than a five minute job.
--
-- This function is idempotent, so the worker can call it on a schedule to keep
-- future partitions ready ahead of time.
CREATE OR REPLACE FUNCTION ensure_ledger_partitions(
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

        FOREACH parent IN ARRAY ARRAY['journal_entry', 'posting'] LOOP
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

-- Bootstrap a year of partitions so a fresh environment can accept writes
-- immediately, without waiting for the worker's first run.
SELECT ensure_ledger_partitions(1, 12);
