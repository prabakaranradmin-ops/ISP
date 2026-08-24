-- +goose Up
-- General ledger, Phase 1 (CRD-EXP-006 | DBD §6.2 "General ledger") — a real
-- chart of accounts and double-entry journal for the business's own books,
-- distinct from wallet_ledgers (one subscriber's prepaid balance) and
-- lco_ledger (one franchise partner's commission). Neither of those answers
-- "what did the business itself earn and spend" — this does.
--
-- Standalone by design: no foreign key from any existing table points here,
-- and nothing existing is touched. Auto-posting from wallet recharges,
-- franchise commission, or received purchase orders is Phase 2, scoped
-- separately (see the CRD) precisely because it would touch three already-
-- correct live financial code paths — this migration does not attempt it.

CREATE TABLE IF NOT EXISTS chart_of_accounts (
    id             SERIAL       PRIMARY KEY,
    code           VARCHAR(20)  NOT NULL UNIQUE,
    name           VARCHAR(100) NOT NULL,
    account_type   VARCHAR(20)  NOT NULL
                       CHECK (account_type IN ('asset', 'liability', 'equity', 'income', 'expense')),
    -- Stored rather than derived from account_type so a report never has to
    -- re-decide it per row, and so a chart of accounts can (rarely, and
    -- deliberately) hold a contra account whose normal side differs from
    -- its type's usual one.
    normal_balance VARCHAR(6)   NOT NULL
                       CHECK (normal_balance IN ('debit', 'credit')),
    is_active      BOOLEAN      NOT NULL DEFAULT true,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- A minimal starter chart. Phase 2's own integrations (wallet recharge,
-- franchise commission, purchase orders) are named here now so that future
-- work only has to add postings, not accounts.
INSERT INTO chart_of_accounts (code, name, account_type, normal_balance) VALUES
    ('1000', 'Cash / Bank',                  'asset',     'debit'),
    ('1200', 'Subscriber Wallet Liability',  'liability', 'credit'),
    ('2000', 'Accounts Payable',             'liability', 'credit'),
    ('3000', 'Owner''s Equity',              'equity',    'credit'),
    ('4000', 'Subscription Revenue',         'income',    'credit'),
    ('5000', 'Franchise Commission Expense', 'expense',   'debit'),
    ('5100', 'Operating Expenses',           'expense',   'debit')
ON CONFLICT (code) DO NOTHING;

CREATE TABLE IF NOT EXISTS gl_journal_entries (
    id           SERIAL       PRIMARY KEY,
    entry_date   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    description  TEXT         NOT NULL,
    -- 'manual' is the only value Phase 1 ever writes. Phase 2 would add
    -- 'wallet_recharge', 'lco_commission', 'purchase_order' — not
    -- constrained by a CHECK here on purpose, so that addition is a Go-level
    -- change, not a migration, when it happens.
    source_type  VARCHAR(30)  NOT NULL DEFAULT 'manual',
    source_id    INTEGER,
    created_by   VARCHAR(100) NOT NULL,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS gl_journal_lines (
    id                SERIAL         PRIMARY KEY,
    journal_entry_id  INTEGER        NOT NULL REFERENCES gl_journal_entries(id) ON DELETE CASCADE,
    account_id        INTEGER        NOT NULL REFERENCES chart_of_accounts(id),
    debit             NUMERIC(14,2)  NOT NULL DEFAULT 0 CHECK (debit >= 0),
    credit            NUMERIC(14,2)  NOT NULL DEFAULT 0 CHECK (credit >= 0),

    -- A line is a debit or a credit, never both — a single line trying to be
    -- both would let a transposition error cancel itself out silently.
    CONSTRAINT chk_gl_line_not_both CHECK (NOT (debit > 0 AND credit > 0)),
    -- No zero-amount no-op line.
    CONSTRAINT chk_gl_line_nonzero  CHECK (debit > 0 OR credit > 0)
);

CREATE INDEX IF NOT EXISTS idx_gl_journal_lines_entry ON gl_journal_lines (journal_entry_id);
CREATE INDEX IF NOT EXISTS idx_gl_journal_lines_account ON gl_journal_lines (account_id);

-- The actual "balanced entry" guarantee. A per-row CHECK cannot express
-- "every line belonging to the same journal_entry_id sums to zero" — that
-- needs a cross-row aggregate, which is exactly what a constraint trigger is
-- for. Deferred to end-of-transaction rather than immediate: an application
-- posting a multi-line entry inserts its lines one at a time, and an
-- immediate check would reject every entry right after its first line, long
-- before the balancing line exists to make it pass.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION check_gl_journal_balanced() RETURNS trigger AS $$
DECLARE
    unbalanced_id INTEGER;
    imbalance NUMERIC(14,2);
BEGIN
    SELECT journal_entry_id, SUM(debit) - SUM(credit)
      INTO unbalanced_id, imbalance
      FROM gl_journal_lines
     WHERE journal_entry_id = COALESCE(NEW.journal_entry_id, OLD.journal_entry_id)
     GROUP BY journal_entry_id
    HAVING SUM(debit) - SUM(credit) <> 0;

    IF unbalanced_id IS NOT NULL THEN
        RAISE EXCEPTION 'gl_journal_entries %: debits and credits differ by %', unbalanced_id, imbalance;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER trg_gl_journal_balanced
    AFTER INSERT OR UPDATE OR DELETE ON gl_journal_lines
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION check_gl_journal_balanced();

-- Trial balance: every account's debit/credit totals and a signed balance
-- oriented by the account's own normal_balance, so an account reads
-- positive when it is in its expected position and negative when it is
-- not — exactly the anomaly a trial balance exists to surface.
CREATE OR REPLACE VIEW v_gl_trial_balance AS
SELECT
    a.id AS account_id,
    a.code,
    a.name,
    a.account_type,
    a.normal_balance,
    COALESCE(SUM(l.debit), 0)  AS debit_total,
    COALESCE(SUM(l.credit), 0) AS credit_total,
    CASE WHEN a.normal_balance = 'debit'
         THEN COALESCE(SUM(l.debit), 0) - COALESCE(SUM(l.credit), 0)
         ELSE COALESCE(SUM(l.credit), 0) - COALESCE(SUM(l.debit), 0)
    END AS balance
FROM chart_of_accounts a
LEFT JOIN gl_journal_lines l ON l.account_id = a.id
GROUP BY a.id, a.code, a.name, a.account_type, a.normal_balance;

-- Income statement (P&L) and balance sheet both need a date window/point in
-- time, which a plain CREATE VIEW cannot parameterise — functions returning
-- a table stand in for a parameterised view here.
-- Both functions below sum via a CASE *inside* the aggregate rather than
-- filtering the date range in the join's ON clause (which was this
-- migration's first draft, and wrong): gl_journal_lines.journal_entry_id is
-- NOT NULL and always resolves to a real row in gl_journal_entries, so a
-- date condition on that second join only ever nulls out e's own columns —
-- it never removes l's row from the join, and a plain SUM(l.credit) would
-- silently total every line for the account across all of history,
-- regardless of the requested window. Filtering in WHERE instead has the
-- opposite failure: it drops an account's row entirely (rather than
-- reporting zero) whenever every one of its lines happens to fall outside
-- the window, breaking a stable "every account, defaulting to zero" report.
-- A CASE inside SUM is the only one of the three that gets both right.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION gl_income_statement(from_date TIMESTAMPTZ, to_date TIMESTAMPTZ)
RETURNS TABLE(account_id INTEGER, code VARCHAR, name VARCHAR, account_type VARCHAR, amount NUMERIC) AS $$
    SELECT a.id, a.code, a.name, a.account_type,
           CASE WHEN a.normal_balance = 'credit'
                THEN COALESCE(SUM(CASE WHEN e.entry_date >= from_date AND e.entry_date < to_date THEN l.credit END), 0)
                   - COALESCE(SUM(CASE WHEN e.entry_date >= from_date AND e.entry_date < to_date THEN l.debit END), 0)
                ELSE COALESCE(SUM(CASE WHEN e.entry_date >= from_date AND e.entry_date < to_date THEN l.debit END), 0)
                   - COALESCE(SUM(CASE WHEN e.entry_date >= from_date AND e.entry_date < to_date THEN l.credit END), 0)
           END AS amount
      FROM chart_of_accounts a
      LEFT JOIN gl_journal_lines l ON l.account_id = a.id
      LEFT JOIN gl_journal_entries e ON e.id = l.journal_entry_id
     WHERE a.account_type IN ('income', 'expense')
     GROUP BY a.id, a.code, a.name, a.account_type;
$$ LANGUAGE sql STABLE;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION gl_balance_sheet(as_of TIMESTAMPTZ)
RETURNS TABLE(account_id INTEGER, code VARCHAR, name VARCHAR, account_type VARCHAR, amount NUMERIC) AS $$
    SELECT a.id, a.code, a.name, a.account_type,
           CASE WHEN a.normal_balance = 'credit'
                THEN COALESCE(SUM(CASE WHEN e.entry_date < as_of THEN l.credit END), 0)
                   - COALESCE(SUM(CASE WHEN e.entry_date < as_of THEN l.debit END), 0)
                ELSE COALESCE(SUM(CASE WHEN e.entry_date < as_of THEN l.debit END), 0)
                   - COALESCE(SUM(CASE WHEN e.entry_date < as_of THEN l.credit END), 0)
           END AS amount
      FROM chart_of_accounts a
      LEFT JOIN gl_journal_lines l ON l.account_id = a.id
      LEFT JOIN gl_journal_entries e ON e.id = l.journal_entry_id
     WHERE a.account_type IN ('asset', 'liability', 'equity')
     GROUP BY a.id, a.code, a.name, a.account_type;
$$ LANGUAGE sql STABLE;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS gl_balance_sheet(TIMESTAMPTZ);
DROP FUNCTION IF EXISTS gl_income_statement(TIMESTAMPTZ, TIMESTAMPTZ);
DROP VIEW IF EXISTS v_gl_trial_balance;
DROP TRIGGER IF EXISTS trg_gl_journal_balanced ON gl_journal_lines;
DROP FUNCTION IF EXISTS check_gl_journal_balanced();
DROP TABLE IF EXISTS gl_journal_lines;
DROP TABLE IF EXISTS gl_journal_entries;
DROP TABLE IF EXISTS chart_of_accounts;
