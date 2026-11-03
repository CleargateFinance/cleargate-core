-- Counterparties: whoever an agent pays (whitepaper §4.1, §6.2;
-- data-architecture.md §4.4).
--
-- A counterparty is its own entity, never a column on an account. It is
-- created by traffic, before the payee has heard of Cleargate, and a payee
-- that signs up later claims it and inherits the history. account_id is that
-- claim, nullable, and is not the counterparty's identity.
--
-- Identity, the owner's decision of 3 November 2026: the endpoint the agent
-- reports paying for, normalised to host and path. Its registrable domain and
-- host are kept beside it, the two levels data-architecture.md §4.4 scores
-- distrust at. When the agent reports no usable URL, the payee is identified
-- by the address the money went to on its network. The raw pay_to and
-- resource_url stay on every payment, so this table can be rebuilt if the
-- rule ever changes.

CREATE TABLE counterparty (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    kind          text NOT NULL CHECK (kind IN ('endpoint', 'address')),

    -- An endpoint: lower-case host (with any non-default port), and the path.
    -- Query and fragment are never part of it, since they vary per request
    -- and can carry secrets.
    domain        text CHECK (char_length(domain) <= 253),
    host          text CHECK (char_length(host) <= 259),
    path          text CHECK (char_length(path) <= 2048 AND path LIKE '/%'),

    -- An address: the CAIP-2 network and the address exactly as given, since
    -- whether case matters depends on the chain.
    network       text CHECK (network ~ '^[-a-z0-9]{3,8}:[-_a-zA-Z0-9]{1,32}$'),
    address       text CHECK (char_length(address) BETWEEN 1 AND 256),

    -- Derived by the database, so the key can never disagree with the
    -- columns it names. Unique, so one identity is one counterparty.
    canonical_key text GENERATED ALWAYS AS (
                      CASE kind
                          WHEN 'endpoint' THEN 'endpoint:' || host || path
                          ELSE 'address:' || network || ':' || address
                      END
                  ) STORED UNIQUE,

    -- The claim by a payee that signed up (not in the MVP).
    account_id    uuid REFERENCES account (id),
    claimed_at    timestamptz,

    first_seen_at timestamptz NOT NULL,

    CHECK (kind <> 'endpoint' OR (domain IS NOT NULL AND host IS NOT NULL AND path IS NOT NULL
                                  AND network IS NULL AND address IS NULL)),
    CHECK (kind <> 'address' OR (network IS NOT NULL AND address IS NOT NULL
                                 AND domain IS NULL AND host IS NULL AND path IS NULL)),
    -- Lower case, so API.Example.com and api.example.com cannot be two payees.
    CHECK (host = lower(host) AND domain = lower(domain)),
    CHECK ((account_id IS NULL) = (claimed_at IS NULL))
);

-- What the directory and its claim flow will list by: a domain's endpoints.
CREATE INDEX counterparty_by_domain ON counterparty (domain) WHERE kind = 'endpoint';

-- A counterparty's identity never changes and it is never deleted: payments
-- and, later, its reputation point at it. Only a claim may be added, once.
CREATE OR REPLACE FUNCTION counterparty_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'counterparty is never deleted';
    END IF;

    IF (NEW.id, NEW.kind, NEW.domain, NEW.host, NEW.path, NEW.network, NEW.address, NEW.first_seen_at)
       IS DISTINCT FROM
       (OLD.id, OLD.kind, OLD.domain, OLD.host, OLD.path, OLD.network, OLD.address, OLD.first_seen_at)
    THEN
        RAISE EXCEPTION 'a counterparty''s identity is immutable';
    END IF;

    IF OLD.account_id IS NOT NULL
       AND (NEW.account_id, NEW.claimed_at) IS DISTINCT FROM (OLD.account_id, OLD.claimed_at)
    THEN
        RAISE EXCEPTION 'a counterparty is claimed once';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER counterparty_guard
    BEFORE UPDATE OR DELETE ON counterparty
    FOR EACH ROW EXECUTE FUNCTION counterparty_guard();

-- ---------------------------------------------------------------------------
-- Payments name their counterparty
-- ---------------------------------------------------------------------------

-- Null when the payment named nothing to identify a payee by: no usable URL
-- and no address. Such a payment can still be judged and declined.
ALTER TABLE payment ADD COLUMN counterparty_id uuid REFERENCES counterparty (id);

CREATE INDEX payment_by_counterparty ON payment (counterparty_id, created_at DESC)
    WHERE counterparty_id IS NOT NULL;

-- 000007's guard, with counterparty_id among the columns that never change:
-- who a payment was made to is part of its request.
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
        NEW.counterparty_id, NEW.decided_at, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.account_id, OLD.agent_id, OLD.amount, OLD.asset, OLD.category,
        OLD.pay_to, OLD.resource_url, OLD.rail, OLD.token, OLD.network,
        OLD.counterparty_id, OLD.decided_at, OLD.created_at)
    THEN
        RAISE EXCEPTION 'a payment''s request is immutable, only its status moves';
    END IF;

    IF NOT (OLD.status = 'authorized' AND NEW.status IN ('settled', 'expired', 'failed')) THEN
        RAISE EXCEPTION 'payment cannot move from % to %', OLD.status, NEW.status;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
