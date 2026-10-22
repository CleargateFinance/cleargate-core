-- Identity, accounts, and the humans and agents that act within them.
--
-- Four actors:
--   principal    a legal party: a person or a legal entity
--   account      a customer's account, owned by a principal
--   app_user     a human login, exactly one per principal
--   agent        a non-human actor that spends against a signed mandate
--
-- Everything else in this file is either how a human proves who they are
-- (sessions, MFA, WebAuthn, tokens), how account access is granted and
-- guarded (membership, invitations, control changes), or how an agent
-- authenticates and is grouped into runs.

-- ---------------------------------------------------------------------------
-- Identity
-- ---------------------------------------------------------------------------

CREATE TABLE principal (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind text NOT NULL CHECK (kind IN ('person', 'legal_entity')),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Compliance identity: who this party is in law.
CREATE TABLE principal_identity (
    principal_id            uuid PRIMARY KEY REFERENCES principal(id),
    legal_name              text,
    country                 text CHECK (country ~ '^[A-Z]{2}$'),   -- ISO 3166-1 alpha-2
    kyc_status              text NOT NULL DEFAULT 'none'
                                CHECK (kyc_status IN ('none','pending','verified','rejected','expired')),
    kyc_provider            text,          -- 'moonpay' | 'transak'
    kyc_reference           text,          -- the provider's verification id, for audit
    kyc_verified_at         timestamptz,
    erased_at               timestamptz,
    retention_hold_until    date,     -- AML retention; blocks erasure once money moved

    -- A verified status without a timestamp is a bug which never should happen.
    CHECK (kyc_status <> 'verified' OR kyc_verified_at IS NOT NULL)
);

-- ---------------------------------------------------------------------------
-- Login
-- ---------------------------------------------------------------------------

-- A login. Exactly one per principal. Note: the email is an authentication
-- identifier, not compliance data, which is why it lives here.
--
-- Declared before account so that account.frozen_by can reference it.
CREATE TABLE app_user (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  principal_id        uuid NOT NULL UNIQUE REFERENCES principal(id),
  email               text NOT NULL UNIQUE CHECK (email = lower(email)),
  email_verified_at   timestamptz,
  password_hash       text NOT NULL,   -- argon2id, PHC string
  password_changed_at timestamptz NOT NULL DEFAULT now(),
  status              text NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active','suspended','deactivated')),
  created_at          timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- Accounts
-- ---------------------------------------------------------------------------

CREATE TABLE account (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  principal_id      uuid NOT NULL REFERENCES principal(id),
  parent_account_id uuid REFERENCES account(id),   -- for platform tiering
  display_name      text NOT NULL,
  status            text NOT NULL DEFAULT 'active'
                      CHECK (status IN ('active','suspended','closed')),

  -- Who froze the account, so that person can lift it instantly while anyone
  -- else has to wait.
  frozen_by         uuid REFERENCES app_user(id),
  frozen_at         timestamptz,

  -- How long a risk-increasing change waits before it takes effect. Zero is
  -- the default and means no delay, which is correct for an account with no
  -- guardian: a waiting period protects nobody when nobody is watching it.
  timelock_hours    int NOT NULL DEFAULT 0 CHECK (timelock_hours BETWEEN 0 AND 168),

  created_at        timestamptz NOT NULL DEFAULT now(),

  -- Freezing (frozen_at) is the customer's call and suspension (status) the
  -- platform's. They're kept apart so unfreezing can't lift a suspension. An
  -- account may spend only when status is 'active' and frozen_at is NULL.
  CONSTRAINT account_frozen_by_needs_frozen_at
    CHECK (frozen_by IS NULL OR frozen_at IS NOT NULL)
);

-- ---------------------------------------------------------------------------
-- Account membership and roles
-- ---------------------------------------------------------------------------

CREATE TABLE account_membership (
  account_id uuid NOT NULL REFERENCES account(id),
  user_id    uuid NOT NULL REFERENCES app_user(id),
  role       text NOT NULL CHECK (role IN ('owner','admin','guardian','viewer')),
  invited_by uuid REFERENCES app_user(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (account_id, user_id)
);

-- Exactly one owner and at most one guardian per account at any time.
CREATE UNIQUE INDEX account_one_owner ON account_membership (account_id)
  WHERE role = 'owner';
CREATE UNIQUE INDEX account_one_guardian ON account_membership (account_id)
  WHERE role = 'guardian';

-- ---------------------------------------------------------------------------
-- Sessions
-- ---------------------------------------------------------------------------

-- Server-side sessions. The token is never stored, only its digest.
CREATE TABLE user_session (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id          uuid NOT NULL REFERENCES app_user(id),
  token_hash       bytea NOT NULL UNIQUE,   -- sha256 of 256 random bits
  mfa_satisfied_at timestamptz,             -- drives step-up
  issued_at        timestamptz NOT NULL DEFAULT now(),
  last_seen_at     timestamptz NOT NULL DEFAULT now(),
  expires_at       timestamptz NOT NULL,
  revoked_at       timestamptz,
  ip               inet,
  user_agent       text
);
CREATE INDEX user_session_live ON user_session (user_id) WHERE revoked_at IS NULL;

-- ---------------------------------------------------------------------------
-- Multi-factor authentication
-- ---------------------------------------------------------------------------

CREATE TABLE user_mfa_factor (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id        uuid NOT NULL REFERENCES app_user(id),
  type           text NOT NULL CHECK (type IN ('totp')),
  secret_enc     bytea NOT NULL,   -- encrypted, not hashed: verification needs it back
  last_used_step bigint,           -- replay guard
  confirmed_at   timestamptz,
  created_at     timestamptz NOT NULL DEFAULT now(),
  revoked_at     timestamptz
);
CREATE UNIQUE INDEX user_mfa_one_active ON user_mfa_factor (user_id, type)
  WHERE confirmed_at IS NOT NULL AND revoked_at IS NULL;

CREATE TABLE user_recovery_code (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id    uuid NOT NULL REFERENCES app_user(id),
  code_hash  text NOT NULL,        -- argon2id; these are human-typed, so brute-forceable
  used_at    timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- WebAuthn credentials and challenges
-- ---------------------------------------------------------------------------

-- Public keys only. The private half lives inside the authenticator's
-- hardware, is not exportable, and never reaches us.
CREATE TABLE user_webauthn_credential (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id         uuid NOT NULL REFERENCES app_user(id),
  credential_id   bytea NOT NULL UNIQUE,
  public_key      bytea NOT NULL,          -- COSE-encoded
  sign_count      bigint NOT NULL DEFAULT 0,
  aaguid          uuid,                    -- authenticator model
  transports      text[],
  name            text NOT NULL,           -- e.g. "Anton's MacBook"
  backup_eligible boolean NOT NULL,        -- true for a cloud-synced passkey
  backup_state    boolean NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now(),
  last_used_at    timestamptz,
  revoked_at      timestamptz
);
CREATE INDEX user_webauthn_live ON user_webauthn_credential (user_id)
  WHERE revoked_at IS NULL;

-- Server-issued, single-use, short-lived. `bound_to` names what the assertion
-- authorises, so a signature collected for one mandate cannot be replayed
-- onto another.
CREATE TABLE webauthn_challenge (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id     uuid NOT NULL REFERENCES app_user(id),
  challenge   bytea NOT NULL UNIQUE,
  purpose     text NOT NULL CHECK (purpose IN
                ('enrol','sign_mandate','confirm_control_change')),
  bound_to    uuid,
  expires_at  timestamptz NOT NULL,
  consumed_at timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now(),

  CHECK (purpose = 'enrol' OR bound_to IS NOT NULL)
);

-- ---------------------------------------------------------------------------
-- Verification and reset tokens
-- ---------------------------------------------------------------------------

-- Single-use, expiring, hashed. One table because the lifecycle is identical.
CREATE TABLE user_token (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id     uuid NOT NULL REFERENCES app_user(id),
  purpose     text NOT NULL CHECK (purpose IN ('email_verify','password_reset')),
  token_hash  bytea NOT NULL UNIQUE,
  expires_at  timestamptz NOT NULL,
  consumed_at timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX user_token_live ON user_token (user_id, purpose) WHERE consumed_at IS NULL;

-- ---------------------------------------------------------------------------
-- Invitations
-- ---------------------------------------------------------------------------

CREATE TABLE account_invitation (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id  uuid NOT NULL REFERENCES account(id),
  email       text NOT NULL CHECK (email = lower(email)),
  role        text NOT NULL CHECK (role IN ('admin','guardian','viewer')),
  invited_by  uuid NOT NULL REFERENCES app_user(id),
  token_hash  bytea NOT NULL UNIQUE,
  expires_at  timestamptz NOT NULL,
  accepted_at timestamptz,
  revoked_at  timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX account_invitation_pending
  ON account_invitation (account_id, email)
  WHERE accepted_at IS NULL AND revoked_at IS NULL;

-- ---------------------------------------------------------------------------
-- Account control changes
-- ---------------------------------------------------------------------------

-- Changes that alter the safety net itself, rather than acting within it.
-- Owner-proposed, delayed, notified, and applied unless stopped. Which of them
-- may be vetoed, and by whom, is application policy: nobody may veto their own
-- removal as guardian, or the control becomes a lockout.
CREATE TABLE account_control_change (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id   uuid NOT NULL REFERENCES account(id),
  change_type  text NOT NULL CHECK (change_type IN
                 ('transfer_owner','reassign_guardian','remove_guardian',
                  'lower_timelock','close_account')),
  payload      jsonb NOT NULL,   -- {"user_id": ...} or {"timelock_hours": ...}
  proposed_by  uuid NOT NULL REFERENCES app_user(id),

  status       text NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending','applied','vetoed','cancelled')),

  -- When it lands if nobody stops it. Fixed at proposal time from the timelock
  -- then in force, and never recomputed: otherwise shortening the timelock
  -- would accelerate a proposal already in flight.
  effective_at timestamptz NOT NULL,

  applied_at   timestamptz,
  vetoed_by    uuid REFERENCES app_user(id),
  vetoed_at    timestamptz,
  cancelled_at timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now(),

  CHECK ((status = 'vetoed')    = (vetoed_at IS NOT NULL)),
  CHECK ((status = 'applied')   = (applied_at IS NOT NULL)),
  CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL))
);

-- One live proposal of each kind at a time.
CREATE UNIQUE INDEX account_control_change_live
  ON account_control_change (account_id, change_type)
  WHERE status = 'pending';

-- ---------------------------------------------------------------------------
-- Notifications
-- ---------------------------------------------------------------------------

-- What was sent, to whom, about what. Emission rides the outbox in the same
-- transaction as the change itself; this records the delivery outcome, because
-- a guardian whose mail bounces has silently stopped being a control.
CREATE TABLE notification (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id      uuid NOT NULL REFERENCES app_user(id),
  account_id   uuid REFERENCES account(id),
  kind         text NOT NULL,     -- 'mandate.proposed', 'guardian.removal_proposed', ...
  subject_type text,              -- 'mandate' | 'account_control_change' | 'agent'
  subject_id   uuid,
  channel      text NOT NULL DEFAULT 'email' CHECK (channel IN ('email')),
  created_at   timestamptz NOT NULL DEFAULT now(),
  sent_at      timestamptz,
  failed_at    timestamptz,
  failure      text
);
CREATE INDEX notification_by_user ON notification (user_id, created_at DESC);
CREATE INDEX notification_failed ON notification (account_id) WHERE failed_at IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Audit trail and consent
-- ---------------------------------------------------------------------------

-- Every authentication outcome, including failures against unknown addresses.
CREATE TABLE auth_event (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id    uuid REFERENCES app_user(id),   -- NULL when the email is unknown
  email      text,
  type       text NOT NULL CHECK (type IN
               ('signup','email_verified','login_succeeded','login_failed',
                'mfa_enrolled','mfa_succeeded','mfa_failed','recovery_code_used',
                'password_reset_requested','password_changed',
                'session_revoked','invite_accepted')),
  ip         inet,
  user_agent text,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX auth_event_by_user ON auth_event (user_id, created_at DESC);

CREATE TABLE user_consent (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id     uuid NOT NULL REFERENCES app_user(id),
  document    text NOT NULL CHECK (document IN ('terms_of_service','privacy_policy')),
  version     text NOT NULL,
  accepted_at timestamptz NOT NULL DEFAULT now(),
  ip          inet,
  UNIQUE (user_id, document, version)
);

-- ---------------------------------------------------------------------------
-- Agents
-- ---------------------------------------------------------------------------

-- A registration, not a program. The agent runs on the customer's own
-- infrastructure; this row is the identity it is connected to Cleargate as,
-- and the thing a credential authenticates and a mandate authorises.
CREATE TABLE agent (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  account_id uuid NOT NULL REFERENCES account(id),
  name       text NOT NULL,
  status     text NOT NULL DEFAULT 'active' CHECK (status IN ('active','revoked')),
  created_by uuid REFERENCES app_user(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  revoked_at timestamptz,

  -- Status and timestamp cannot disagree about whether this agent can spend.
  CHECK ((status = 'revoked') = (revoked_at IS NOT NULL))
);

CREATE TABLE agent_credential (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  agent_id     uuid NOT NULL REFERENCES agent(id),
  name         text NOT NULL,          -- "prod-worker-3"; which key to revoke at 2am
  prefix       text NOT NULL UNIQUE,   -- lookup key, safe to log
  secret_hash  bytea NOT NULL,         -- sha256; the secret is 256 CSPRNG bits
  created_by   uuid REFERENCES app_user(id),
  created_at   timestamptz NOT NULL DEFAULT now(),
  revoked_at   timestamptz,
  last_used_at timestamptz
);

-- Groups payments into one agent run. Required for per-session features.
-- Cannot be inferred retroactively.
CREATE TABLE agent_session (
  id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  agent_id   uuid NOT NULL REFERENCES agent(id),
  task_ref   text,
  task_text  text,
  started_at timestamptz NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- On-ramp mapping
-- ---------------------------------------------------------------------------

-- Maps the on-ramp provider's customer to our principal. Without this the
-- funding webhook knows a payment cleared but not whom to credit.
CREATE TABLE onramp_customer (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  principal_id uuid NOT NULL REFERENCES principal(id),
  provider     text NOT NULL CHECK (provider IN ('moonpay','transak')),
  external_id  text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (provider, external_id)
);

-- ---------------------------------------------------------------------------
-- Foreign keys from earlier migrations
-- ---------------------------------------------------------------------------

-- The ledger and the spend counters were created before account and agent
-- existed, so their foreign keys are added here.
ALTER TABLE ledger_account
  ADD CONSTRAINT ledger_account_account_fk
    FOREIGN KEY (account_id) REFERENCES account(id);

ALTER TABLE agent_spend_counter
  ADD CONSTRAINT agent_spend_counter_agent_fk
    FOREIGN KEY (agent_id) REFERENCES agent(id);
