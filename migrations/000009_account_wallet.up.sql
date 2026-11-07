-- Account wallets: the on-chain address that holds an account's USDC
-- (whitepaper §8.8, §9.3; Task 7.5).
--
-- One wallet per account per chain, never per agent. Agents hold spend
-- counters against the account's pooled balance (data-architecture.md §4.6),
-- so a per-agent wallet would strand capital and multiply keys. The primary
-- key is what enforces that, and chain is in it so Base can add a second
-- wallet to the same account without a new key.
--
-- The keys stay with the provider (Coinbase CDP for the MVP, as Server
-- Wallets, the owner's decision of 5 November 2026). This table holds only
-- the public address: it is what a deposit is matched to its account by
-- (Phase 8), and what the deposit-address route reads without a network call.

CREATE TABLE account_wallet (
    account_id uuid NOT NULL REFERENCES account (id),
    chain      text NOT NULL CHECK (chain IN ('solana')),

    -- Who holds the key. A second provider is a new value, not a new table.
    provider   text NOT NULL CHECK (provider IN ('cdp')),

    -- A Solana address is a base58 Ed25519 public key: 32 to 44 characters
    -- from an alphabet with no 0, O, I or l. Unique, so a deposit can never
    -- match two accounts.
    address    text NOT NULL UNIQUE
                    CHECK (chain <> 'solana' OR address ~ '^[1-9A-HJ-NP-Za-km-z]{32,44}$'),

    created_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (account_id, chain),

    -- Lets a deposit name its wallet and account together, so the two cannot
    -- disagree about whose money arrived.
    CONSTRAINT account_wallet_account_address_unique UNIQUE (account_id, address)
);

-- A wallet is never changed or removed. Deposits were matched to this
-- address, and the provider still holds its key: repointing an account at
-- another address would credit the wrong account for every deposit to the
-- old one still in flight.
CREATE OR REPLACE FUNCTION account_wallet_guard() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'account_wallet is append-only, % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER account_wallet_guard
    BEFORE UPDATE OR DELETE ON account_wallet
    FOR EACH ROW EXECUTE FUNCTION account_wallet_guard();
