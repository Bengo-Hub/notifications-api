-- Daily summaries instead of one message per event (Hadia owner gift digest, platform task N3).
-- Create "digest_items" table
CREATE TABLE "digest_items" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "kind" character varying NOT NULL, "recipient_key" character varying NOT NULL, "local_date" character varying NOT NULL, "group_key" character varying NOT NULL DEFAULT '', "event_id" character varying NOT NULL, "amount_minor" bigint NOT NULL DEFAULT 0, "data" jsonb NULL, "send_id" uuid NULL, "created_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "digestitem_tenant_kind_event" to table: "digest_items"
CREATE UNIQUE INDEX "digestitem_tenant_kind_event" ON "digest_items" ("tenant_id", "kind", "event_id");
-- Create index "digestitem_pending" to table: "digest_items"
CREATE INDEX "digestitem_pending" ON "digest_items" ("kind", "tenant_id", "recipient_key", "local_date") WHERE (send_id IS NULL);
-- Create index "digestitem_send_id" to table: "digest_items"
CREATE INDEX "digestitem_send_id" ON "digest_items" ("send_id");
-- Create index "digestitem_created_at" to table: "digest_items"
CREATE INDEX "digestitem_created_at" ON "digest_items" ("created_at");
-- Create "digest_sends" table
CREATE TABLE "digest_sends" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "kind" character varying NOT NULL, "recipient_key" character varying NOT NULL, "local_date" character varying NOT NULL, "status" character varying NOT NULL DEFAULT 'sending', "channels" jsonb NULL, "item_count" bigint NOT NULL DEFAULT 0, "claimed_at" timestamptz NOT NULL, "sent_at" timestamptz NULL, "created_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "digestsend_once_per_day" to table: "digest_sends"
CREATE UNIQUE INDEX "digestsend_once_per_day" ON "digest_sends" ("tenant_id", "kind", "recipient_key", "local_date");
-- Create index "digestsend_created_at" to table: "digest_sends"
CREATE INDEX "digestsend_created_at" ON "digest_sends" ("created_at");
