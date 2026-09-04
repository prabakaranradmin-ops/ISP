-- +goose Up
-- Payment precedes authorisation (FR-BIL-009, MDS 4.14).
--
-- No creation path set plan_expiry, and both billing scanners filter on
-- plan_expiry IS NOT NULL, so a subscriber created through the API or the
-- console was never invoiced, never reminded and never suspended. RADIUS
-- authorises on status alone and status was 'active' from creation, so they
-- were online indefinitely, for free. On this install four subscribers had
-- been in that state for eleven days.
--
-- The fix is a starting status that grants nothing. A new subscriber is
-- created 'pending_payment' and stays there until money arrives and the
-- first cycle is actually charged, at which point the auto-renewal scanner
-- activates them and stamps plan_expiry.
ALTER TABLE subscribers DROP CONSTRAINT subscribers_status_check;
ALTER TABLE subscribers ADD  CONSTRAINT subscribers_status_check
    CHECK (status IN ('pending_payment','active','grace_period','soft_suspended',
                      'hard_suspended','terminated'));

-- Backfill, expressed as rules rather than as the ids that happen to be
-- affected here, so a longer-running install gets the same treatment.

-- Never billed: no invoice, no ledger movement, no expiry. Whatever service
-- they have had was unbilled, so they go back to where signup should have
-- left them. A subscriber who somehow carries a balance is included
-- deliberately: the activation pass will pick them up on its next tick and
-- charge them properly, which is a better outcome than leaving them
-- indefinitely in a state nothing collects from.
UPDATE subscribers s
   SET status = 'pending_payment'
 WHERE s.plan_expiry IS NULL
   AND s.status <> 'terminated'
   AND NOT EXISTS (SELECT 1 FROM invoices       i WHERE i.subscriber_id = s.id)
   AND NOT EXISTS (SELECT 1 FROM wallet_ledgers w WHERE w.subscriber_id = s.id);

-- Billed at some point but left with no expiry: they paid, so give them the
-- cycle that payment bought, dated from signup, and let the dunning scanner
-- carry them forward from there. validity_days rather than a flat 30 so a
-- subscriber on a quarterly or annual plan is not silently shortened.
UPDATE subscribers s
   SET plan_expiry = s.created_at + (p.validity_days || ' days')::interval
  FROM plans p
 WHERE p.id = s.plan_id
   AND s.plan_expiry IS NULL
   AND s.status <> 'terminated'
   AND (EXISTS (SELECT 1 FROM invoices       i WHERE i.subscriber_id = s.id)
     OR EXISTS (SELECT 1 FROM wallet_ledgers w WHERE w.subscriber_id = s.id));

-- The invariant the whole defect reduces to: nobody may hold a status that
-- gets them onto the network while being invisible to the billing scanners.
--
-- Both scanners require plan_expiry IS NOT NULL, and radius.AuthorisesService
-- grants access on status alone, so the combination "authorising status, no
-- expiry" is free service with nothing that can ever come to collect. That is
-- exactly the state four subscribers here were in.
--
-- Enforced in the database rather than in the provisioning path because the
-- provisioning path was only one of the ways in. An operator setting status
-- back to 'active' through the console or a bulk action reaches the same
-- state, as does a hand-written UPDATE during an incident. A CHECK is the one
-- place that covers all of them, including the ones not written yet.
--
-- The exempt statuses are precisely those radius.AuthorisesService refuses;
-- the two lists have to be read together, which is why each names the other.
ALTER TABLE subscribers ADD CONSTRAINT chk_authorised_subscriber_is_billable
    CHECK (status IN ('pending_payment','hard_suspended','terminated')
           OR plan_expiry IS NOT NULL);

-- +goose Down
ALTER TABLE subscribers DROP CONSTRAINT chk_authorised_subscriber_is_billable;
-- Nothing can stay in a status the restored constraint does not allow.
UPDATE subscribers SET status = 'active' WHERE status = 'pending_payment';
ALTER TABLE subscribers DROP CONSTRAINT subscribers_status_check;
ALTER TABLE subscribers ADD  CONSTRAINT subscribers_status_check
    CHECK (status IN ('active','grace_period','soft_suspended',
                      'hard_suspended','terminated'));
