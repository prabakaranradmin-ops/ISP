-- scripts/seed_local.sql
-- Local development seed data — DXD §11.4
-- Run after all migrations: psql -h localhost -U postgres -d isp_bss_oss -f scripts/seed_local.sql
-- Insertion order respects FK dependencies: franchises → plans → subscribers → rest

-- ── GST Rates ────────────────────────────────────────────────────────────────
INSERT INTO gst_rates (cgst_rate, sgst_rate, igst_rate, effective_from)
VALUES (9.00, 9.00, 18.00, NOW())
ON CONFLICT DO NOTHING;

-- ── Plans ────────────────────────────────────────────────────────────────────
INSERT INTO plans (name, rate_limit_string, volume_gb, fup_threshold_bytes, fup_throttle_string, price, validity_days)
VALUES
    ('TN_Basic_50M',  '50M/50M',   1650, 1771674009600, '5M/5M',   499.00, 30),
    ('TN_Super_100M', '100M/100M', 3300, 3543348019200, '10M/10M', 799.00, 30),
    ('TN_Ultra_200M', '200M/200M', 5000, 5368709120000, '20M/20M', 1199.00, 30)
ON CONFLICT DO NOTHING;

-- ── Notification Templates ───────────────────────────────────────────────────
INSERT INTO notification_templates (id, channel, template_name, event_trigger, active)
VALUES
    ('TMPL-001', 'whatsapp', 'fup_warning',          'fup_warning',       TRUE),
    ('TMPL-002', 'whatsapp', 'fup_throttle_applied',  'fup_throttle',      TRUE),
    ('TMPL-003', 'whatsapp', 'renewal_reminder',      'renewal_reminder',  TRUE),
    ('TMPL-004', 'whatsapp', 'soft_suspension',       'soft_suspension',   TRUE),
    ('TMPL-005', 'whatsapp', 'hard_suspension',       'hard_suspension',   TRUE),
    ('TMPL-006', 'whatsapp', 'service_restored',      'service_restored',  TRUE),
    ('TMPL-007', 'whatsapp', 'payment_received',      'payment_received',  TRUE),
    ('TMPL-008', 'whatsapp', 'ticket_update',         'ticket_update',     TRUE)
ON CONFLICT (id) DO NOTHING;

-- ── Subscribers ──────────────────────────────────────────────────────────────
-- password_hash values are bcrypt(cost=12) of "testpassword" — NEVER use in production
--
-- test_user carries a plan_expiry because migration 048's
-- chk_authorised_subscriber_is_billable refuses an active subscriber without
-- one; before this column was set here, the whole seed failed on this row.
-- suspended_user is hard_suspended, which that constraint exempts.
INSERT INTO subscribers
    (caf_number, username, password_hash, mobile_number, plan_id, status, wallet_balance, registered_state, dunning_state, plan_expiry)
VALUES
    ('CAF-0001', 'test_user',
     '$2a$12$gn685/ch8wxn9XO33DI4lei.c/vdxKTXTg1VV0Cty2qHQLnnfnHbq',
     '+919876543210',
     (SELECT id FROM plans WHERE name = 'TN_Super_100M'),
     'active', 799.00, 'TN', 'active', NOW() + INTERVAL '30 days'),

    ('CAF-0002', 'suspended_user',
     '$2a$12$gn685/ch8wxn9XO33DI4lei.c/vdxKTXTg1VV0Cty2qHQLnnfnHbq',
     '+919876543211',
     (SELECT id FROM plans WHERE name = 'TN_Basic_50M'),
     'hard_suspended', 0.00, 'TN', 'active', NULL)
ON CONFLICT (username) DO NOTHING;

-- ── Wallet Ledger (initial credit for test_user) ─────────────────────────────
INSERT INTO wallet_ledgers (subscriber_id, entry_type, amount, balance_after, transaction_token, description)
SELECT id, 'credit', 799.00, 799.00, 'seed_tok_001', 'Initial seed credit'
FROM subscribers WHERE username = 'test_user'
ON CONFLICT DO NOTHING;

-- ── One Test Invoice ─────────────────────────────────────────────────────────
INSERT INTO invoices (subscriber_id, base_amount, cgst_amount, sgst_amount, igst_amount, total_amount, gst_rate_id, gb_included, gb_used)
SELECT
    s.id,
    799.00,
    71.91,
    71.91,
    0.00,
    942.82,
    g.id,
    3300,
    0.00
FROM subscribers s, gst_rates g
WHERE s.username = 'test_user'
ORDER BY g.id LIMIT 1
ON CONFLICT DO NOTHING;

-- ── Past sessions, so the Usage page has something to show ───────────────────
--
-- Without these the portal's Usage page renders its "no session history yet"
-- empty state, which is correct but leaves testers unable to judge the screen
-- that matters most to a subscriber. Four closed sessions give the page a
-- realistic shape: a long overnight session, two short ones, and yesterday's.
--
-- start_time is clamped into the current month on purpose. This table is
-- RANGE-partitioned by start_time and migration 011 creates the current month
-- plus three future ones — a row dated into last month would fail to route to
-- any partition and take the whole seed down with it on the 1st or 2nd of a
-- month.
INSERT INTO subscriber_session_history (
    subscriber_id, session_id, nas_ip_address, assigned_ipv4,
    start_time, stop_time, input_octets, output_octets, terminate_cause
)
SELECT
    s.id,
    'demo-sess-' || v.n,
    '10.10.0.1'::inet,
    ('100.64.0.' || (10 + v.n))::inet,
    GREATEST(date_trunc('month', NOW()) + INTERVAL '1 hour', NOW() - (v.days_ago * INTERVAL '1 day')),
    GREATEST(date_trunc('month', NOW()) + INTERVAL '1 hour', NOW() - (v.days_ago * INTERVAL '1 day')) + (v.hours * INTERVAL '1 hour'),
    v.in_octets,
    v.out_octets,
    'User-Request'
FROM subscribers s
CROSS JOIN (VALUES
    (1, 6, 9,  412000000000::bigint, 138000000000::bigint),
    (2, 4, 3,   96000000000::bigint,  31000000000::bigint),
    (3, 3, 2,   54000000000::bigint,  18000000000::bigint),
    (4, 1, 7,  301000000000::bigint,  99000000000::bigint)
) AS v(n, days_ago, hours, in_octets, out_octets)
WHERE s.username = 'test_user'
ON CONFLICT DO NOTHING;

-- ── Console operators, one per CRD persona ───────────────────────────────────
--
-- All five share the password 'staffpassword' for the demo. Real deployments
-- create these through an administrator, not a seed file — this exists so the
-- operations console can be opened and used immediately after demo_up.sh.
--
-- lea_access is granted only to the NOC engineer and the owner, and is a
-- separate column from the role on purpose (SecD 9.3): reach over
-- law-enforcement lookups must never arrive as a side effect of a job title.
INSERT INTO staff_users (username, password_hash, full_name, role, lea_access) VALUES
    ('owner',   '$2a$12$Lbf0AtykY18fe1C5QMX7B.RtijYgxeBX.iqh5UKTWdPbIRTIUpyP2', 'Priya Raman (ISP Owner)',      'isp_owner',     TRUE),
    ('noc',     '$2a$12$Lbf0AtykY18fe1C5QMX7B.RtijYgxeBX.iqh5UKTWdPbIRTIUpyP2', 'Arun Kumar (NOC Engineer)',    'noc_engineer',  TRUE),
    ('billing', '$2a$12$Lbf0AtykY18fe1C5QMX7B.RtijYgxeBX.iqh5UKTWdPbIRTIUpyP2', 'Meena Iyer (Billing Admin)',   'billing_admin', FALSE),
    ('csr',     '$2a$12$Lbf0AtykY18fe1C5QMX7B.RtijYgxeBX.iqh5UKTWdPbIRTIUpyP2', 'Divya Nair (CSR)',             'csr',           FALSE),
    ('tech',    '$2a$12$Lbf0AtykY18fe1C5QMX7B.RtijYgxeBX.iqh5UKTWdPbIRTIUpyP2', 'Suresh Babu (Ground Tech)',    'technician',    FALSE)
ON CONFLICT (username) DO NOTHING;

-- ── Verify seed counts ────────────────────────────────────────────────────────
SELECT 'plans'                  AS table_name, COUNT(*) AS row_count FROM plans
UNION ALL
SELECT 'gst_rates',              COUNT(*) FROM gst_rates
UNION ALL
SELECT 'subscribers',            COUNT(*) FROM subscribers
UNION ALL
SELECT 'notification_templates', COUNT(*) FROM notification_templates
UNION ALL
SELECT 'wallet_ledgers',         COUNT(*) FROM wallet_ledgers
UNION ALL
SELECT 'invoices',               COUNT(*) FROM invoices
UNION ALL
SELECT 'session_history',        COUNT(*) FROM subscriber_session_history
UNION ALL
SELECT 'staff_users',            COUNT(*) FROM staff_users;
