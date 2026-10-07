-- Modify "announcements" table
ALTER TABLE "announcements" ADD COLUMN "tenant_id" uuid NULL;
-- Create index "announcement_tenant_id_is_active" to table: "announcements"
CREATE INDEX "announcement_tenant_id_is_active" ON "announcements" ("tenant_id", "is_active");
-- Modify "delivery_logs" table
ALTER TABLE "delivery_logs" ADD COLUMN "message_id" character varying NULL;
-- Drop index "deliverylog_tenant_id" from table: "delivery_logs" ((tenant_id, created_at) covers it)
DROP INDEX IF EXISTS "deliverylog_tenant_id";
-- Create index "deliverylog_message_id" to table: "delivery_logs"
CREATE INDEX "deliverylog_message_id" ON "delivery_logs" ("message_id");
-- Create "broadcasts" table
CREATE TABLE "broadcasts" ("id" uuid NOT NULL, "scope" character varying NOT NULL, "tenant_id" uuid NULL, "kind" character varying NOT NULL DEFAULT 'announcement', "class" character varying NOT NULL DEFAULT 'marketing', "status" character varying NOT NULL DEFAULT 'draft', "title" character varying NOT NULL, "channels" jsonb NOT NULL, "content" jsonb NOT NULL, "audience" jsonb NOT NULL, "send_at" timestamptz NULL, "occasion_id" uuid NULL, "occasion_year" bigint NULL, "requested_by" character varying NULL, "approved_by" character varying NULL, "approved_at" timestamptz NULL, "target_count" bigint NOT NULL DEFAULT 0, "sent_count" bigint NOT NULL DEFAULT 0, "failed_count" bigint NOT NULL DEFAULT 0, "skipped_count" bigint NOT NULL DEFAULT 0, "suppressed_count" bigint NOT NULL DEFAULT 0, "metadata" jsonb NULL, "completed_at" timestamptz NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "broadcast_platform_occasion_year" to table: "broadcasts"
CREATE UNIQUE INDEX "broadcast_platform_occasion_year" ON "broadcasts" ("occasion_id", "occasion_year") WHERE ((tenant_id IS NULL) AND (occasion_id IS NOT NULL));
-- Create index "broadcast_status_send_at" to table: "broadcasts"
CREATE INDEX "broadcast_status_send_at" ON "broadcasts" ("status", "send_at");
-- Create index "broadcast_tenant_id_status_send_at" to table: "broadcasts"
CREATE INDEX "broadcast_tenant_id_status_send_at" ON "broadcasts" ("tenant_id", "status", "send_at");
-- Create index "broadcast_tenant_occasion_year" to table: "broadcasts"
CREATE UNIQUE INDEX "broadcast_tenant_occasion_year" ON "broadcasts" ("tenant_id", "occasion_id", "occasion_year") WHERE ((tenant_id IS NOT NULL) AND (occasion_id IS NOT NULL));
-- Create "broadcast_recipients" table
CREATE TABLE "broadcast_recipients" ("id" uuid NOT NULL, "broadcast_id" uuid NOT NULL, "tenant_id" uuid NULL, "recipient_tenant_id" uuid NULL, "channel" character varying NOT NULL, "address" character varying NULL, "address_hash" character varying NOT NULL, "display_name" character varying NULL, "vars" jsonb NULL, "status" character varying NOT NULL DEFAULT 'pending', "provider_message_id" character varying NULL, "error" character varying NULL, "attempts" bigint NOT NULL DEFAULT 0, "next_attempt_at" timestamptz NULL, "sent_at" timestamptz NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "broadcastrecipient_broadcast_id_channel_address_hash" to table: "broadcast_recipients"
CREATE UNIQUE INDEX "broadcastrecipient_broadcast_id_channel_address_hash" ON "broadcast_recipients" ("broadcast_id", "channel", "address_hash");
-- Create index "broadcastrecipient_broadcast_id_status_id" to table: "broadcast_recipients"
CREATE INDEX "broadcastrecipient_broadcast_id_status_id" ON "broadcast_recipients" ("broadcast_id", "status", "id");
-- Create index "broadcastrecipient_created_at" to table: "broadcast_recipients"
CREATE INDEX "broadcastrecipient_created_at" ON "broadcast_recipients" ("created_at");
-- Create index "broadcastrecipient_provider_message_id" to table: "broadcast_recipients"
CREATE INDEX "broadcastrecipient_provider_message_id" ON "broadcast_recipients" ("provider_message_id");
-- Create "occasions" table
CREATE TABLE "occasions" ("id" uuid NOT NULL, "tenant_id" uuid NULL, "key" character varying NOT NULL, "name" character varying NOT NULL, "country" character varying NOT NULL DEFAULT 'KE', "rule" jsonb NOT NULL, "duration_days" bigint NOT NULL DEFAULT 1, "lead_days" bigint NOT NULL DEFAULT 14, "send_offset_days" bigint NOT NULL DEFAULT 0, "is_active" boolean NOT NULL DEFAULT true, "metadata" jsonb NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "occasion_platform_key" to table: "occasions"
CREATE UNIQUE INDEX "occasion_platform_key" ON "occasions" ("key") WHERE (tenant_id IS NULL);
-- Create index "occasion_tenant_key" to table: "occasions"
CREATE UNIQUE INDEX "occasion_tenant_key" ON "occasions" ("tenant_id", "key") WHERE (tenant_id IS NOT NULL);
-- Create "suppressions" table
CREATE TABLE "suppressions" ("id" uuid NOT NULL, "tenant_id" uuid NULL, "channel" character varying NOT NULL, "address_hash" character varying NOT NULL, "scope" character varying NOT NULL DEFAULT 'marketing', "reason" character varying NOT NULL DEFAULT 'unsubscribe', "source" character varying NULL, "created_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "suppression_platform_address" to table: "suppressions"
CREATE UNIQUE INDEX "suppression_platform_address" ON "suppressions" ("channel", "address_hash", "scope") WHERE (tenant_id IS NULL);
-- Create index "suppression_tenant_address" to table: "suppressions"
CREATE UNIQUE INDEX "suppression_tenant_address" ON "suppressions" ("tenant_id", "channel", "address_hash", "scope") WHERE (tenant_id IS NOT NULL);
