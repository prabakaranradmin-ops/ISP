-- +goose Up
-- Seed the notification templates (FR-NOTIF-009, IDX traceability table).
--
-- notification_templates has been empty since it was created. Nothing broke
-- loudly, because CreateNotificationLog resolved template_id through
-- (SELECT id FROM notification_templates WHERE id = $3) — a subquery that
-- returns NULL for a row that is not there, into a nullable column documented
-- as "system events may have none". So every notification this system has
-- ever sent logged template_id NULL, and the audit log cannot answer which
-- template any of them used.
--
-- That empty table is also what let the second defect sit unnoticed. It is
-- the only thing binding the ids the code sends to the templates the spec
-- defines, and with no rows there was nothing to disagree with. The code had
-- drifted: it sent TMPL-005 for a payment receipt, which the traceability
-- index assigns to *hard suspension*, and TMPL-006 (Service restored) was
-- bound to an unrelated "plan_expiring" and never sent at all. Against real
-- Meta templates registered to the spec, a paying customer would have been
-- sent a suspension notice.
--
-- The ids below are the traceability index's, which is authoritative.
INSERT INTO notification_templates (id, channel, template_name, event_trigger, active) VALUES
    ('TMPL-001', 'whatsapp', 'fup_warning_80pct',      'fup_warning_80pct',       true),
    ('TMPL-002', 'whatsapp', 'fup_throttled',          'fup_throttled',           true),
    ('TMPL-003', 'whatsapp', 'payment_reminder',       'dunning_reminder',        true),
    ('TMPL-004', 'whatsapp', 'service_suspended_soft', 'dunning_soft_suspended',  true),
    ('TMPL-005', 'whatsapp', 'service_suspended_hard', 'dunning_hard_suspended',  true),
    ('TMPL-006', 'whatsapp', 'service_restored',       'service_restored',        true),
    ('TMPL-007', 'whatsapp', 'payment_received',       'payment_received',        true),
    ('TMPL-008', 'whatsapp', 'ticket_update',          'ticket_update',           true)
ON CONFLICT (id) DO NOTHING;

-- Make the delete rule explicit. The foreign key already existed with the
-- default NO ACTION, which behaves the same here, but a template that has
-- been sent to somebody is part of the audit trail and the schema should say
-- outright that it cannot be deleted out from under it.
ALTER TABLE notification_log DROP CONSTRAINT notification_log_template_id_fkey;
ALTER TABLE notification_log ADD  CONSTRAINT notification_log_template_id_fkey
    FOREIGN KEY (template_id) REFERENCES notification_templates(id) ON DELETE RESTRICT;

-- +goose Down
ALTER TABLE notification_log DROP CONSTRAINT notification_log_template_id_fkey;
ALTER TABLE notification_log ADD  CONSTRAINT notification_log_template_id_fkey
    FOREIGN KEY (template_id) REFERENCES notification_templates(id);
-- Only the seeded rows, and only if nothing has been logged against them.
DELETE FROM notification_templates t
 WHERE t.id IN ('TMPL-001','TMPL-002','TMPL-003','TMPL-004',
                'TMPL-005','TMPL-006','TMPL-007','TMPL-008')
   AND NOT EXISTS (SELECT 1 FROM notification_log l WHERE l.template_id = t.id);
