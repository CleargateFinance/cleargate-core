-- Reverses 000007_payment.up.sql. Tables go before the functions their
-- triggers use; partitions go with their parent. reject_mutation belongs to
-- 000002 and stays.

DROP TABLE IF EXISTS event_outbox;
DROP TABLE IF EXISTS idempotency_key;
DROP TABLE IF EXISTS decision_check;
DROP TABLE IF EXISTS decision;
DROP TABLE IF EXISTS payment;

DROP FUNCTION IF EXISTS event_outbox_guard();
DROP FUNCTION IF EXISTS ensure_decision_partitions(int, int);
DROP FUNCTION IF EXISTS assert_payment_decided();
DROP FUNCTION IF EXISTS payment_guard();

ALTER TABLE mandate DROP CONSTRAINT IF EXISTS mandate_evidence_unique;
ALTER TABLE agent DROP CONSTRAINT IF EXISTS agent_id_account_unique;
