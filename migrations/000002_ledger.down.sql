-- Reverses 000002_ledger.up.sql.
--
-- Dropping a partitioned table drops all of its partitions with it, so the
-- monthly tables created by ensure_ledger_partitions need no separate handling.
-- posting is dropped before journal_entry because it references it.

DROP FUNCTION IF EXISTS ensure_ledger_partitions(int, int);

DROP TABLE IF EXISTS posting;
DROP TABLE IF EXISTS journal_entry;
DROP TABLE IF EXISTS ledger_account;

DROP FUNCTION IF EXISTS assert_entry_balanced();
DROP FUNCTION IF EXISTS reject_mutation();
