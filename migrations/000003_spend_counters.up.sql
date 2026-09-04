-- Per-agent spend meters.
--
-- Agents do not hold money, they hold spending authority. The money stays
-- pooled in the account's customer_funds bucket, and each agent's mandate
-- carries limits that draw against that shared pool. This is the corporate
-- card model: giving an employee a monthly limit does not move that amount
-- into a separate account for them.
--
-- So this table holds counters, never balances. A counter records how much an
-- agent has spent in a period, which is what enforces a cap. It holds no funds
-- of its own, and per-agent remaining amounts may legitimately sum to more
-- than the account balance, exactly as several employees' card limits may.

CREATE TABLE agent_spend_counter (
    agent_id     uuid NOT NULL,

    -- The window this counter covers. Daily and monthly caps are tracked
    -- separately, so one agent has one row per active window per asset.
    period_type  text NOT NULL CHECK (period_type IN ('day', 'month')),
    period_start date NOT NULL,

    asset        text NOT NULL,

    -- Spending only ever accumulates within a window, so this can never go
    -- negative. A refund is attributed to the period it is issued in rather
    -- than clawing back a closed window.
    spent        numeric(38, 18) NOT NULL DEFAULT 0 CHECK (spent >= 0),

    updated_at   timestamptz NOT NULL DEFAULT now(),

    -- The composite key is what makes the increment a single atomic upsert.
    -- Reading a total in application code and writing back total + amount
    -- would let two concurrent payments both read the same stale figure and
    -- both pass a cap that only one of them fits under.
    PRIMARY KEY (agent_id, period_type, period_start, asset)
);

-- Supports the reconciliation job, which walks every counter in a window and
-- compares it against the postings it was derived from.
CREATE INDEX agent_spend_counter_by_period
    ON agent_spend_counter (period_type, period_start);

-- No foreign key to an agent table yet, because that table arrives with the
-- account model. It is added there rather than left dangling here.
