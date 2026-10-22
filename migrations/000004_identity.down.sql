-- Reverses 000004_identity.up.sql.
--
-- The foreign keys this migration added to earlier tables go first. After
-- that, tables are dropped in reverse creation order, which is safe because
-- every remaining foreign key points at a table created earlier.
-- Indexes are dropped automatically with their table.

ALTER TABLE agent_spend_counter DROP CONSTRAINT IF EXISTS agent_spend_counter_agent_fk;
ALTER TABLE ledger_account DROP CONSTRAINT IF EXISTS ledger_account_account_fk;

DROP TABLE IF EXISTS onramp_customer;

DROP TABLE IF EXISTS agent_session;
DROP TABLE IF EXISTS agent_credential;
DROP TABLE IF EXISTS agent;

DROP TABLE IF EXISTS user_consent;
DROP TABLE IF EXISTS auth_event;

DROP TABLE IF EXISTS notification;

DROP TABLE IF EXISTS account_control_change;

DROP TABLE IF EXISTS account_invitation;

DROP TABLE IF EXISTS user_token;

DROP TABLE IF EXISTS webauthn_challenge;
DROP TABLE IF EXISTS user_webauthn_credential;

DROP TABLE IF EXISTS user_recovery_code;
DROP TABLE IF EXISTS user_mfa_factor;

DROP TABLE IF EXISTS user_session;

DROP TABLE IF EXISTS account_membership;

-- account before app_user: account.frozen_by references it.
DROP TABLE IF EXISTS account;

DROP TABLE IF EXISTS app_user;

DROP TABLE IF EXISTS principal_identity;
DROP TABLE IF EXISTS principal;
