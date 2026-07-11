-- reverse: create "tenant_fence" table
DROP TABLE "tenant_fence";
-- reverse: create "tenant_event_seq" table
DROP TABLE "tenant_event_seq";
-- reverse: create "tenant_event_policy" table
DROP TABLE "tenant_event_policy";
-- reverse: create "outbox_dispatch_cursor" table
DROP TABLE "outbox_dispatch_cursor";
-- reverse: create index "idx_outbox_dead_letter_cursor_name" to table: "outbox_dead_letter"
DROP INDEX "idx_outbox_dead_letter_cursor_name";
-- reverse: create "outbox_dead_letter" table
DROP TABLE "outbox_dead_letter";
-- reverse: create index "idx_outbox_event_epoch" to table: "outbox"
DROP INDEX "idx_outbox_event_epoch";
-- reverse: create index "idx_outbox_created_time" to table: "outbox"
DROP INDEX "idx_outbox_created_time";
-- reverse: create index "idx_outbox_account_id" to table: "outbox"
DROP INDEX "idx_outbox_account_id";
-- reverse: create "outbox" table
DROP TABLE "outbox";
-- reverse: create "idempotency_markers" table
DROP TABLE "idempotency_markers";
-- reverse: create index "idx_idempotency_keys_expires_at" to table: "idempotency_keys"
DROP INDEX "idx_idempotency_keys_expires_at";
-- reverse: create "idempotency_keys" table
DROP TABLE "idempotency_keys";
