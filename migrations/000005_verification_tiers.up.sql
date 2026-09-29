-- Verification tiers, address screening, and the funding limits they gate.
--
-- The rule this migration exists to enforce: what a principal may spend is
-- gated by a verification TIER, never by how their money arrived. Tying
-- verification to funding method produces the wrong shape — a $20 card top-up
-- would face more friction than $20,000 in stablecoin.
--
-- Two concepts that are constantly conflated and are kept apart here:
--
--   verification   who this party is in law. Identity documents, legal entity,
--                  beneficial ownership. Attaches to a PRINCIPAL.
--   screening      whether a blockchain address is associated with sanctioned
--                  or criminal activity. Says nothing about who anybody is.
--                  Attaches to an ADDRESS.
--
-- A principal can be fully screened and completely unverified. That is the
-- entire basis of the low-cap trial: no identity documents, but no unscreened
-- money either.

-- ---------------------------------------------------------------------------
-- Verification tier
-- ---------------------------------------------------------------------------

ALTER TABLE principal_identity
  ADD COLUMN verification_tier text NOT NULL DEFAULT 'basic'
    CHECK (verification_tier IN ('basic','verified','business'));

-- A tier above basic is a claim that identity was actually verified, so it
-- cannot exist without a verified KYC status. Enforced here rather than in a
-- service, because every future funding path inherits it for free and none of
-- them can forget it.
ALTER TABLE principal_identity
  ADD CONSTRAINT principal_identity_tier_requires_kyc
    CHECK (verification_tier = 'basic' OR kyc_status = 'verified');

COMMENT ON COLUMN principal_identity.verification_tier IS
  'What this principal is entitled to do. basic: low cap, no identity documents, '
  'screened funding only. verified: identity confirmed, standard limits. '
  'business: legal entity and beneficial ownership, high limits.';

-- ---------------------------------------------------------------------------
-- Address screening
-- ---------------------------------------------------------------------------

-- Every address that funds an account or receives a withdrawal is screened.
-- This is what makes the no-verification trial non-reckless rather than merely
-- non-compliant: AML responsibility remains ours whether or not we asked for
-- identity documents.
CREATE TABLE screened_address (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  principal_id uuid NOT NULL REFERENCES principal(id),
  chain        text NOT NULL,
  address      text NOT NULL,

  status       text NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending','clear','flagged','rejected')),
  provider     text,           -- 'cdp'
  reference    text,           -- the provider's screening id, for audit
  screened_at  timestamptz,

  -- Set on the first deposit that arrives from this address. It is what makes
  -- return-to-source withdrawal possible: an unverified principal may only
  -- withdraw back to an address that actually funded them.
  first_funded_at timestamptz,

  created_at   timestamptz NOT NULL DEFAULT now(),

  -- One screening record per address per principal. The same address funding
  -- two principals is two records, deliberately, because the verdict is about
  -- this party's use of it.
  UNIQUE (principal_id, chain, address),

  -- A decided verdict always carries its timestamp.
  CHECK (status = 'pending' OR screened_at IS NOT NULL)
);

CREATE INDEX screened_address_by_principal
  ON screened_address (principal_id) WHERE status = 'clear';

-- ---------------------------------------------------------------------------
-- On-ramp providers
-- ---------------------------------------------------------------------------

-- Coinbase joins the existing providers, since CDP bundles an on-ramp and the
-- wallet provider is Coinbase CDP.
ALTER TABLE onramp_customer
  DROP CONSTRAINT onramp_customer_provider_check;

ALTER TABLE onramp_customer
  ADD CONSTRAINT onramp_customer_provider_check
    CHECK (provider IN ('moonpay','transak','coinbase'));
