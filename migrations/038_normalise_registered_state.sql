-- +goose Up
-- Session DB-038 | FR-BIL-001, FR-BIL-006 | DDS §5.4
--
-- Normalise subscribers.registered_state to the two-letter code the GST
-- calculation actually compares against, and add the GSTIN needed to tell a
-- B2B supply from a B2C one.
--
-- Why the normalisation matters more than it looks. registered_state was
-- free text validated only as non-empty, and CalculateGstInvoice compared
-- it to the literal 'TN'. A Tamil Nadu subscriber recorded as 'Tamil Nadu'
-- therefore fell through to the interstate branch and was charged IGST
-- rather than CGST+SGST. The invoice total is identical (18% either as 9+9
-- or as 18), so nothing looked wrong on any invoice or in any total - but
-- IGST accrues wholly to the centre while CGST/SGST is shared with the
-- state, so the tax was filed under the wrong head. Rows written before
-- this migration keep whatever spelling was typed, so they have to be
-- rewritten here; validation at the API only stops new ones.
--
-- Invoices already issued are deliberately left alone. Their cgst/sgst/igst
-- columns record what was actually charged and filed, and rewriting history
-- to what should have been charged would put the ledger out of step with
-- returns already submitted. Correcting those is an accounting exercise
-- (a credit note and a re-issue), not a data migration.

-- Only the spellings that can actually occur are mapped: the two-letter
-- code (already canonical), the full state name, and the numeric GST code.
-- Anything else is left untouched rather than guessed at - a value nobody
-- anticipated should surface as an unrecognised state at the next
-- calculation, not be silently rounded to a neighbour.
-- +goose StatementBegin
DO $$
DECLARE
    mapping CONSTANT text[][] := ARRAY[
        ['JK','01','JAMMU AND KASHMIR'], ['HP','02','HIMACHAL PRADESH'],
        ['PB','03','PUNJAB'],            ['CH','04','CHANDIGARH'],
        ['UK','05','UTTARAKHAND'],       ['HR','06','HARYANA'],
        ['DL','07','DELHI'],             ['RJ','08','RAJASTHAN'],
        ['UP','09','UTTAR PRADESH'],     ['BR','10','BIHAR'],
        ['SK','11','SIKKIM'],            ['AR','12','ARUNACHAL PRADESH'],
        ['NL','13','NAGALAND'],          ['MN','14','MANIPUR'],
        ['MZ','15','MIZORAM'],           ['TR','16','TRIPURA'],
        ['ML','17','MEGHALAYA'],         ['AS','18','ASSAM'],
        ['WB','19','WEST BENGAL'],       ['JH','20','JHARKHAND'],
        ['OD','21','ODISHA'],            ['CG','22','CHHATTISGARH'],
        ['MP','23','MADHYA PRADESH'],    ['GJ','24','GUJARAT'],
        ['DD','25','DAMAN AND DIU'],
        ['DN','26','DADRA AND NAGAR HAVELI AND DAMAN AND DIU'],
        ['MH','27','MAHARASHTRA'],       ['KA','29','KARNATAKA'],
        ['GA','30','GOA'],               ['LD','31','LAKSHADWEEP'],
        ['KL','32','KERALA'],            ['TN','33','TAMIL NADU'],
        ['PY','34','PUDUCHERRY'],        ['AN','35','ANDAMAN AND NICOBAR ISLANDS'],
        ['TS','36','TELANGANA'],         ['AP','37','ANDHRA PRADESH'],
        ['LA','38','LADAKH'],            ['OT','97','OTHER TERRITORY']
    ];
    i int;
    code text;
    gst  text;
    name text;
BEGIN
    FOR i IN 1 .. array_length(mapping, 1) LOOP
        code := mapping[i][1];
        gst  := mapping[i][2];
        name := mapping[i][3];

        -- Compared with spaces stripped and case folded, matching
        -- billing.NormaliseState, so 'tamil nadu', 'TamilNadu' and
        -- 'TAMIL NADU' all resolve the same way here as they do in Go.
        UPDATE subscribers
           SET registered_state = code
         WHERE registered_state <> code
           AND upper(replace(registered_state, ' ', '')) IN (
                 upper(code), gst, upper(replace(name, ' ', ''))
               );
    END LOOP;

    -- Pre-bifurcation Andhra Pradesh. Historical rows may carry 28; new
    -- registrations use 37.
    UPDATE subscribers SET registered_state = 'AP'
     WHERE trim(registered_state) = '28';
END
$$;
-- +goose StatementEnd

-- GSTIN of the *subscriber*, for B2B supplies. NULL is the ordinary case:
-- a residential customer is not registered, and GSTR-1 reports them under
-- B2C. Without this column every supply is B2C by construction, which is
-- why FR-BIL-006's B2B/B2C split could not be produced at all.
--
-- Length 15 is the fixed GSTIN format: 2-digit state code, 10-character
-- PAN, 1 entity digit, 1 filler 'Z', 1 checksum. The CHECK enforces that
-- shape and that the leading two digits agree with the subscriber's own
-- registered state - a GSTIN whose state disagrees with the address it is
-- billed against is the single most common data-entry error here, and it
-- would put the supply in the wrong state's return.
ALTER TABLE subscribers
    ADD COLUMN IF NOT EXISTS gstin VARCHAR(15);

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'chk_subscriber_gstin_format'
    ) THEN
        ALTER TABLE subscribers
            ADD CONSTRAINT chk_subscriber_gstin_format
            CHECK (gstin IS NULL OR gstin ~ '^[0-9]{2}[A-Z]{5}[0-9]{4}[A-Z][0-9A-Z]Z[0-9A-Z]$');
    END IF;
END
$$;
-- +goose StatementEnd

CREATE INDEX IF NOT EXISTS idx_subscribers_gstin ON subscribers(gstin)
    WHERE gstin IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_subscribers_gstin;
ALTER TABLE subscribers DROP CONSTRAINT IF EXISTS chk_subscriber_gstin_format;
ALTER TABLE subscribers DROP COLUMN IF EXISTS gstin;
-- registered_state is deliberately not un-normalised: the original
-- spellings are not recorded anywhere, and restoring 'Tamil Nadu' from
-- 'TN' would be inventing data. The canonical form is also valid input to
-- every version of the calculation, so leaving it is safe.
