CREATE INDEX CONCURRENTLY idx_seat_capacity_outbox_share_join
    ON seat_capacity_outbox(workspace_id, share_link_id, user_id)
    WHERE action = 'claim_share_join' AND dead_lettered_at IS NULL;
