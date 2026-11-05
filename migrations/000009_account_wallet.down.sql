-- Reverses 000009_account_wallet.up.sql: the table first, then the function
-- its trigger uses.

DROP TABLE IF EXISTS account_wallet;
DROP FUNCTION IF EXISTS account_wallet_guard();
