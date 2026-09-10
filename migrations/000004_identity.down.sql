-- Reverses 000004_identity.up.sql.
--
-- Tables are dropped in reverse creation order, which is always safe here
-- because a foreign key can only ever point at a table created earlier.
-- Indexes are dropped automatically with their table.

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
