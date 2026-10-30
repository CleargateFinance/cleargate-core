-- Mandates: signed, versioned grants of spending authority to one agent.
--
-- Whitepaper §4.1, §6.1, data-architecture.md §4.2. A mandate is never
-- edited. Changing an agent's rules inserts version n+1 and closes version n,
-- so a dispute six months later can ask which rules were in force at an
-- instant and get the answer that was true then, not the one that is true now.

CREATE TABLE mandate (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  agent_id       uuid NOT NULL REFERENCES agent(id),
  version        int  NOT NULL CHECK (version > 0),

  -- The exact bytes that were approved, and later signed (Task 3.1). bytea and
  -- not jsonb: jsonb re-serialises on the way out, reordering keys and
  -- dropping whitespace, so the bytes read back would not be the bytes that
  -- were signed and no signature over them would ever verify again.
  payload        bytea NOT NULL,

  -- Also the WebAuthn challenge once Task 3.1 ships. Generated, so it cannot
  -- describe different bytes from the ones stored beside it.
  payload_digest bytea GENERATED ALWAYS AS (sha256(payload)) STORED,

  -- How the mandate was approved. A 'session_stepup' row, the only kind the
  -- MVP writes, was approved by a recent TOTP step-up and has no assertion.
  signature_scheme   text NOT NULL CHECK (signature_scheme IN ('session_stepup', 'webauthn')),

  -- The full assertion, because verification needs all three parts. There is
  -- no server signature here: nothing Cleargate holds can produce one.
  signature          bytea,
  authenticator_data bytea,
  client_data_json   bytea,
  credential_id      bytea REFERENCES user_webauthn_credential(credential_id),
  signed_by          uuid NOT NULL REFERENCES app_user(id),

  status         text NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending', 'active', 'vetoed', 'superseded')),
  vetoed_by      uuid REFERENCES app_user(id),
  vetoed_at      timestamptz,
  effective_from timestamptz NOT NULL,
  effective_to   timestamptz,
  created_at     timestamptz NOT NULL DEFAULT now(),

  -- Queryable projections of the payload, filled in by mandate_derive below
  -- and never written by the application. They exist for queries and the
  -- console; the authorization path parses the payload itself.
  asset          text NOT NULL,
  max_per_tx     numeric(38, 18) NOT NULL,
  max_per_day    numeric(38, 18) NOT NULL,
  max_per_month  numeric(38, 18) NOT NULL,
  categories     text[] NOT NULL,
  pay_to         text[] NOT NULL,
  expires_at     timestamptz NOT NULL,

  -- Caps nest. A per-transaction cap above the daily one can never bind, so a
  -- mandate written that way is a typo rather than a policy.
  CHECK (max_per_tx > 0 AND max_per_tx <= max_per_day AND max_per_day <= max_per_month),
  CHECK (cardinality(categories) > 0),
  CHECK (expires_at > effective_from),

  CHECK ((status = 'vetoed') = (vetoed_at IS NOT NULL)),
  CHECK ((vetoed_by IS NULL) = (vetoed_at IS NULL)),
  CHECK ((status = 'superseded') = (effective_to IS NOT NULL)),
  CHECK (effective_to IS NULL OR effective_to >= effective_from),

  -- A WebAuthn mandate carries its whole assertion, and a step-up mandate
  -- carries none of one, so a half-recorded assertion cannot be mistaken for
  -- either.
  CHECK (signature_scheme <> 'webauthn'
         OR (signature, authenticator_data, client_data_json, credential_id) IS NOT NULL),
  CHECK (signature_scheme <> 'session_stepup'
         OR (signature, authenticator_data, client_data_json, credential_id) IS NULL),

  UNIQUE (agent_id, version)
);

-- Exactly one mandate in force per agent, and at most one proposal waiting.
-- Keyed on status rather than on effective_to IS NULL: a pending mandate has
-- no end date either, and must not count as the one in force.
CREATE UNIQUE INDEX mandate_one_active  ON mandate (agent_id) WHERE status = 'active';
CREATE UNIQUE INDEX mandate_one_pending ON mandate (agent_id) WHERE status = 'pending';

-- ---------------------------------------------------------------------------
-- Projections
-- ---------------------------------------------------------------------------

-- Fills the projection columns from the payload on insert.
--
-- This would be GENERATED ALWAYS like payload_digest, except that reading
-- bytea as text needs convert_from, which Postgres marks STABLE, and generated
-- columns accept only IMMUTABLE expressions. A BEFORE INSERT trigger that
-- overwrites whatever the caller supplied, together with mandate_guard
-- refusing any later change, gives the same guarantee: the queryable columns
-- cannot disagree with the stored bytes.
--
-- It also refuses a payload that names a different agent or version from the
-- row it is stored in, so approved bytes cannot be filed against another
-- agent's history.
CREATE OR REPLACE FUNCTION mandate_derive() RETURNS trigger AS $$
DECLARE
    p jsonb;
BEGIN
    p := convert_from(NEW.payload, 'UTF8')::jsonb;

    IF jsonb_typeof(p) <> 'object' THEN
        RAISE EXCEPTION 'mandate payload must be a JSON object';
    END IF;
    IF (p->>'agent_id')::uuid IS DISTINCT FROM NEW.agent_id THEN
        RAISE EXCEPTION 'mandate payload names agent %, row is for agent %',
            p->>'agent_id', NEW.agent_id;
    END IF;
    IF (p->>'version')::int IS DISTINCT FROM NEW.version THEN
        RAISE EXCEPTION 'mandate payload names version %, row is version %',
            p->>'version', NEW.version;
    END IF;

    NEW.asset         := p->>'asset';
    NEW.max_per_tx    := (p->>'max_per_tx')::numeric;
    NEW.max_per_day   := (p->>'max_per_day')::numeric;
    NEW.max_per_month := (p->>'max_per_month')::numeric;
    NEW.categories    := ARRAY(SELECT jsonb_array_elements_text(p->'categories'));
    NEW.pay_to        := ARRAY(SELECT jsonb_array_elements_text(p->'pay_to'));
    NEW.expires_at    := (p->>'expires_at')::timestamptz;

    -- numeric(38,18) rounds silently. A cap with more precision than that
    -- would be stored as a different number from the one approved.
    IF NEW.max_per_tx    <> (p->>'max_per_tx')::numeric
    OR NEW.max_per_day   <> (p->>'max_per_day')::numeric
    OR NEW.max_per_month <> (p->>'max_per_month')::numeric THEN
        RAISE EXCEPTION 'mandate cap has more than 18 decimal places';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER mandate_derive
    BEFORE INSERT ON mandate
    FOR EACH ROW EXECUTE FUNCTION mandate_derive();

-- ---------------------------------------------------------------------------
-- Immutability
-- ---------------------------------------------------------------------------

-- A mandate's rules and approval never change, and it is never deleted. The
-- only thing that moves is its place in the lifecycle:
--
--   pending -> active       the timelock passed with no veto (Task 2.8)
--   pending -> vetoed       the guardian stopped it (Task 2.8)
--   active  -> superseded   a newer version took over
--
-- Raising rather than silently ignoring the write, as data-architecture.md
-- §4.2's DO INSTEAD NOTHING rule would, for the reason the ledger's
-- reject_mutation gives: a discarded write lets a buggy caller believe it
-- succeeded.
CREATE OR REPLACE FUNCTION mandate_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'mandate is append-only, DELETE is not permitted';
    END IF;

    -- payload_digest is left out: generated columns are not yet computed in a
    -- BEFORE trigger, and payload already covers it.
    IF (NEW.id, NEW.agent_id, NEW.version, NEW.payload, NEW.signature_scheme,
        NEW.signature, NEW.authenticator_data, NEW.client_data_json,
        NEW.credential_id, NEW.signed_by, NEW.effective_from, NEW.created_at,
        NEW.asset, NEW.max_per_tx, NEW.max_per_day, NEW.max_per_month,
        NEW.categories, NEW.pay_to, NEW.expires_at)
       IS DISTINCT FROM
       (OLD.id, OLD.agent_id, OLD.version, OLD.payload, OLD.signature_scheme,
        OLD.signature, OLD.authenticator_data, OLD.client_data_json,
        OLD.credential_id, OLD.signed_by, OLD.effective_from, OLD.created_at,
        OLD.asset, OLD.max_per_tx, OLD.max_per_day, OLD.max_per_month,
        OLD.categories, OLD.pay_to, OLD.expires_at)
    THEN
        RAISE EXCEPTION 'mandate rules are immutable. Insert a new version instead.';
    END IF;

    IF NOT ((OLD.status = 'pending' AND NEW.status IN ('active', 'vetoed'))
         OR (OLD.status = 'active'  AND NEW.status = 'superseded'))
    THEN
        RAISE EXCEPTION 'mandate cannot move from % to %', OLD.status, NEW.status;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER mandate_guard
    BEFORE UPDATE OR DELETE ON mandate
    FOR EACH ROW EXECUTE FUNCTION mandate_guard();
