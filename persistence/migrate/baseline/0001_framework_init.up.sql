-- create "idempotency_markers" table
CREATE TABLE "idempotency_markers" (
  "key" character varying(255) NOT NULL,
  PRIMARY KEY ("key")
);
-- create "outbox" table
CREATE TABLE "outbox" (
  "id" character varying(36) NOT NULL,
  "account_id" text NULL,
  "aggregate_type" text NULL,
  "aggregate_id" text NULL,
  "event_type" text NULL,
  "payload" bytea NULL,
  "created_time" timestamptz NOT NULL,
  "event_seq" bigint NULL DEFAULT 0,
  "event_epoch" bigint NULL DEFAULT 0,
  PRIMARY KEY ("id", "created_time")
);
-- create index "idx_outbox_account_id" to table: "outbox"
CREATE INDEX "idx_outbox_account_id" ON "outbox" ("account_id");
-- create index "idx_outbox_created_time" to table: "outbox"
CREATE INDEX "idx_outbox_created_time" ON "outbox" ("created_time");
-- create index "idx_outbox_event_epoch" to table: "outbox"
CREATE INDEX "idx_outbox_event_epoch" ON "outbox" ("event_epoch");
-- create "outbox_dead_letter" table
CREATE TABLE "outbox_dead_letter" (
  "id" bigserial NOT NULL,
  "cursor_name" text NULL,
  "event_id" text NULL,
  "event_type" text NULL,
  "reason" text NULL,
  "created_time" timestamptz NULL,
  "recorded_at" timestamptz NULL,
  PRIMARY KEY ("id")
);
-- create index "idx_outbox_dead_letter_cursor_name" to table: "outbox_dead_letter"
CREATE INDEX "idx_outbox_dead_letter_cursor_name" ON "outbox_dead_letter" ("cursor_name");
-- create "outbox_dispatch_cursor" table
CREATE TABLE "outbox_dispatch_cursor" (
  "name" character varying(255) NOT NULL,
  "cursor_time" timestamptz NULL,
  "cursor_id" text NULL,
  "head_failures" bigint NULL,
  PRIMARY KEY ("name")
);
-- create "tenant_event_policy" table
CREATE TABLE "tenant_event_policy" (
  "tenant_id" character varying(255) NOT NULL,
  "policy" bigint NULL DEFAULT 0,
  "event_epoch" bigint NULL DEFAULT 0,
  "updated_at" timestamptz NULL,
  PRIMARY KEY ("tenant_id")
);
-- create "tenant_event_seq" table
CREATE TABLE "tenant_event_seq" (
  "account_id" character varying(255) NOT NULL,
  "next_seq" bigint NULL DEFAULT 0,
  PRIMARY KEY ("account_id")
);
-- create "tenant_fence" table
CREATE TABLE "tenant_fence" (
  "tenant_id" character varying(255) NOT NULL,
  "owner_cell" text NULL,
  "route_epoch" bigint NULL DEFAULT 0,
  "sealed" boolean NULL DEFAULT false,
  "barrier_epoch" bigint NULL DEFAULT 0,
  "updated_at" timestamptz NULL,
  PRIMARY KEY ("tenant_id")
);
