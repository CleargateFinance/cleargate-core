-- Reverses 000011_payment_transfer.up.sql: the table first, then the function
-- its trigger uses.

DROP TABLE IF EXISTS payment_transfer;
DROP FUNCTION IF EXISTS payment_transfer_guard();
