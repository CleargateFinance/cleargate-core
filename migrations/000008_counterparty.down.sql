-- Reverses 000008_counterparty.up.sql: 000007's payment guard comes back, then
-- the column and the table go.

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

DROP INDEX IF EXISTS payment_by_counterparty;
ALTER TABLE payment DROP COLUMN IF EXISTS counterparty_id;

DROP TABLE IF EXISTS counterparty;
DROP FUNCTION IF EXISTS counterparty_guard();
