-- Reverses 000005_verification_tiers.up.sql.

ALTER TABLE onramp_customer
  DROP CONSTRAINT onramp_customer_provider_check;

ALTER TABLE onramp_customer
  ADD CONSTRAINT onramp_customer_provider_check
    CHECK (provider IN ('moonpay','transak'));

DROP TABLE IF EXISTS screened_address;

ALTER TABLE principal_identity
  DROP CONSTRAINT IF EXISTS principal_identity_tier_requires_kyc;

ALTER TABLE principal_identity
  DROP COLUMN IF EXISTS verification_tier;
