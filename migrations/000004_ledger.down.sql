-- Reverting drops the whole ledger.
DROP TABLE wallet_ledger_entries;
DROP FUNCTION ledger_forbid_mutation();
ALTER TABLE wager_transactions DROP CONSTRAINT wager_id_wallet_uq;
