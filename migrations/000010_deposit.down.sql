-- Reverses 000010_deposit.up.sql: the tables first, then the function a
-- trigger uses.

DROP TABLE IF EXISTS deposit_cursor;
DROP TABLE IF EXISTS deposit;
DROP FUNCTION IF EXISTS deposit_guard();
