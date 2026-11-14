-- +goose Up
-- Epic 5 (CGNAT / IPFIX auditing): per-flow records decoded from NetFlow v9 by
-- internal/netflow. This is the detail behind a CGNAT abuse complaint — given a
-- public (post-NAT) address, port and time, which private subscriber flow was
-- translated there. The subscriber identity itself is resolved by joining to
-- cgnat_allocations (migration 011) on public_ip + port-range + time window;
-- this table is the flow evidence, kept for the regulatory retention period.
--
-- RANGE partitioned monthly on export_time using create_monthly_partitions()
-- from migration 010, the same mechanism cgnat_allocations and
-- subscriber_session_history use, so retention is enforced by dropping old
-- partitions rather than mass-deleting rows.

CREATE TABLE IF NOT EXISTS flow_records (
    id                 BIGSERIAL    NOT NULL,
    exporter_ip        INET         NOT NULL,          -- the BNG/CGNAT gateway that exported it
    source_id          BIGINT       NOT NULL,          -- NetFlow observation domain
    protocol           SMALLINT     NOT NULL,
    src_addr           INET         NOT NULL,          -- private (pre-NAT) source
    src_port           INTEGER      NOT NULL,
    dst_addr           INET         NOT NULL,
    dst_port           INTEGER      NOT NULL,
    post_nat_src_addr  INET,                           -- public (translated) source
    post_nat_src_port  INTEGER,
    bytes              BIGINT       NOT NULL DEFAULT 0,
    packets            BIGINT       NOT NULL DEFAULT 0,
    flow_start         TIMESTAMPTZ,
    flow_end           TIMESTAMPTZ,
    export_time        TIMESTAMPTZ  NOT NULL,          -- partition key
    PRIMARY KEY (id, export_time)
) PARTITION BY RANGE (export_time);

-- Current month + 3 ahead.
SELECT create_monthly_partitions('flow_records', 3);

-- The abuse/compliance lookup: "who was behind public IP:port at this time".
-- post_nat_src_addr leads because a complaint always starts from the public
-- address; export_time DESC lets the time-bounded scan stop early. The INCLUDE
-- columns make the common answer an index-only scan.
CREATE INDEX idx_flow_cgnat_lookup
    ON flow_records (post_nat_src_addr, post_nat_src_port, export_time DESC)
    INCLUDE (src_addr, src_port, dst_addr, dst_port, protocol);

-- +goose Down
DROP TABLE IF EXISTS flow_records CASCADE;
