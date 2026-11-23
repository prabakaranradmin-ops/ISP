-- +goose Up
-- An automated ledger posting can happen at most once per source event.
--
-- gl_journal_entries has carried source_type and source_id since 043 to tie a
-- posting to whatever caused it, and nothing stopped the same cause being
-- posted twice. Phase 1 could not hit it — manual entries are the only thing
-- that writes here and they leave source_id NULL — but Phase 2's whole job is
-- posting on behalf of a wallet recharge, a commission or a purchase order,
-- and every one of those arrives through a retrying job queue. A retried
-- billing run would have doubled the books, which is the kind of wrong that
-- nobody notices until a trial balance is compared against a bank statement
-- months later.
--
-- Partial, on source_id IS NOT NULL, for two reasons. It states the rule the
-- way it is meant ("an automated posting is unique per source; a manual entry
-- is not constrained") rather than leaning on the reader knowing that
-- PostgreSQL treats NULLs as distinct in a unique index. And it keeps the
-- index to the rows that will actually be looked up by source.
--
-- Verified on a real cluster before being written: three ('manual', NULL)
-- rows are accepted, and a second ('wallet_recharge', 7) is refused.
--
-- No CHECK on source_type, deliberately — 043 says adding a source kind
-- should be a Go-level change rather than a migration, and that still holds.
CREATE UNIQUE INDEX IF NOT EXISTS uq_gl_journal_entries_source
    ON gl_journal_entries (source_type, source_id)
 WHERE source_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS uq_gl_journal_entries_source;
