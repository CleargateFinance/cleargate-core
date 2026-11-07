-- The signed transaction behind an authorized payment (Task 7.6).
--
-- A table of its own rather than columns on payment, which the plan named,
-- because payment_guard holds a payment's request immutable and a transaction
-- is signed after that row commits: Authorize commits first, since signing is a
-- network call. Here the rules are simple. A payment gets at most one, once.
--
-- It is what an idempotent replay returns, so a retry hands the merchant the
-- same transaction rather than a second one that could also land, and what
-- Task 7.7 finds on-chain: a transaction's id is the fee payer's signature,
-- which Cleargate never sees, so it is found by its memo and its message.
CREATE TABLE payment_transfer (
    payment_id uuid PRIMARY KEY REFERENCES payment (id),

    -- On-chain text, either the payment's id or what the merchant set in
    -- extra.memo. A merchant's order reference, not the agent's words, and not
    -- under any hash chain, so it stays erasable (data-architecture.md §8).
    memo text NOT NULL CHECK (octet_length(memo) BETWEEN 1 AND 256),

    -- The wallet that signed, whose history Task 7.7 searches.
    wallet_address text NOT NULL REFERENCES account_wallet (address),

    -- Base64, signed by the account's wallet, the fee payer's slot empty.
    signed_transaction text NOT NULL CHECK (signed_transaction <> ''),

    -- SHA-256 of the message every signer signs. A merchant's memo replaces
    -- the payment id, so two payments of one order on one blockhash sign the
    -- same message. The chain takes it once, and Task 7.7 would settle both
    -- payments from it. Unique, so a message belongs to one payment.
    message_digest bytea NOT NULL UNIQUE CHECK (octet_length(message_digest) = 32),

    -- What the agent sends the merchant as the PAYMENT-SIGNATURE header.
    payment_signature text NOT NULL CHECK (payment_signature <> ''),

    -- The height after which the transaction can never land.
    last_valid_block_height bigint NOT NULL CHECK (last_valid_block_height > 0),

    created_at timestamptz NOT NULL DEFAULT now()
);

-- Only an authorized payment can gain a transaction, and the row lock is what
-- makes that true against Task 7.7 expiring it. The sweep takes FOR UPDATE, so
-- this waits for it, then reads the status it left. A transaction stored here
-- therefore always belongs to a payment the sweep has not already released.
CREATE OR REPLACE FUNCTION payment_transfer_guard() RETURNS trigger AS $$
DECLARE
    current_status text;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'payment_transfer is append-only, % is not permitted', TG_OP;
    END IF;

    SELECT status INTO current_status FROM payment WHERE id = NEW.payment_id FOR SHARE;
    IF current_status IS DISTINCT FROM 'authorized' THEN
        RAISE EXCEPTION 'a % payment cannot gain a transaction', COALESCE(current_status, 'missing')
            USING ERRCODE = 'CG001';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER payment_transfer_guard
    BEFORE INSERT OR UPDATE OR DELETE ON payment_transfer
    FOR EACH ROW EXECUTE FUNCTION payment_transfer_guard();
