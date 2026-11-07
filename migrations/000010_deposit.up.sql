-- Deposits: USDC arriving in an account wallet (Phase 8).
--
-- A deposit is a fact about the chain, recorded once. The ledger entry it
-- credits and this row commit in one transaction, and the primary key is what
-- makes a rescan, a restart or a second worker unable to credit it twice.
--
-- The key is the transaction and the wallet, not the instruction the plan
-- named: an amount is the wallet's net balance change in the transaction
-- (internal/infrastructure/solana), so there is one figure per transaction per
-- wallet, including for a transfer made through another program. One
-- transaction funding two accounts is two rows.

CREATE TABLE deposit (
    -- A CAIP-2 chain id, as on payment.
    network    text NOT NULL CHECK (network ~ '^[-a-z0-9]{3,8}:[-_a-zA-Z0-9]{1,32}$'),

    -- A Solana signature: 64 bytes in base58.
    signature  text NOT NULL CHECK (signature ~ '^[1-9A-HJ-NP-Za-km-z]{64,88}$'),

    account_id uuid NOT NULL,
    address    text NOT NULL,

    -- In the unit of account, at par (§8.3): what the ledger was credited, or
    -- would have been.
    amount     numeric(38, 18) NOT NULL CHECK (amount > 0),
    asset      text NOT NULL CHECK (asset = 'USD'),

    -- credited moved the ledger. quarantined arrived past the account's
    -- funding cap: the tokens sit in the wallet and the balance does not count
    -- them until a person decides. Nothing moves them back automatically.
    status     text NOT NULL CHECK (status IN ('credited', 'quarantined')),

    -- The entry that credited it. No foreign key: the partitioned key would
    -- need that entry's timestamp too.
    journal_entry_id uuid,

    created_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (network, signature, address),
    FOREIGN KEY (account_id, address) REFERENCES account_wallet (account_id, address),

    CHECK ((status = 'credited') = (journal_entry_id IS NOT NULL))
);

CREATE INDEX deposit_by_account ON deposit (account_id, created_at DESC);

-- A deposit is a fact. A correction is a reversing ledger entry and a note
-- beside it, never an edit of what arrived.
CREATE OR REPLACE FUNCTION deposit_guard() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'deposit is append-only, % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER deposit_guard
    BEFORE UPDATE OR DELETE ON deposit
    FOR EACH ROW EXECUTE FUNCTION deposit_guard();

-- How far back a wallet's history has been read, so each poll asks the node
-- only for what is new. It saves calls and decides nothing: losing a row means
-- a rescan, which the primary key above makes harmless. So this is the one
-- table here that is updated.
CREATE TABLE deposit_cursor (
    address    text PRIMARY KEY REFERENCES account_wallet (address),
    signature  text NOT NULL CHECK (signature ~ '^[1-9A-HJ-NP-Za-km-z]{64,88}$'),
    updated_at timestamptz NOT NULL DEFAULT now()
);
