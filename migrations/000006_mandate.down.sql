-- Reverses 000006_mandate.up.sql. The triggers and indexes go with the table;
-- the functions are dropped after it, since the triggers depend on them.

DROP TABLE IF EXISTS mandate;

DROP FUNCTION IF EXISTS mandate_guard();
DROP FUNCTION IF EXISTS mandate_derive();
