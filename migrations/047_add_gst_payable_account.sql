-- +goose Up
-- GST Payable (CRD-EXP-006) — the output-tax liability a renewal creates.
--
-- Until now the chart had no tax account at all, so an auto-renewal posted
-- its entire amount to 4000 Subscription Revenue. Once the renewal charges
-- the tax-inclusive total (the fix in 5c06f2d), that would book the GST
-- portion as income: revenue overstated and liabilities understated by
-- exactly the tax owed to the government, every cycle.
--
-- That error is invisible from inside either system on its own. GSTR-1 is
-- filed from the invoices table, which has the split right, so the return
-- would be correct while the ledger disagreed with it — and the two would
-- never reconcile, because nothing compares them.
--
-- One consolidated account rather than separate CGST/SGST/IGST accounts:
-- the invoices table already stores the three components per invoice, which
-- is the granularity a return needs and the granularity the split is
-- actually reliable at. Three GL accounts would duplicate that split in a
-- second place with no reader for it, and intrastate-vs-interstate is a
-- property of the invoice, not of the ledger.
INSERT INTO chart_of_accounts (code, name, account_type, normal_balance) VALUES
    ('2200', 'GST Payable', 'liability', 'credit')
ON CONFLICT (code) DO NOTHING;

-- +goose Down
DELETE FROM chart_of_accounts WHERE code = '2200';
