-- Modify "delivery_logs" table
ALTER TABLE "delivery_logs" ADD COLUMN "provider_message_id" character varying NULL, ADD COLUMN "error_code" character varying NULL, ADD COLUMN "error_detail" character varying NULL;
-- Create index "deliverylog_provider_message_id" to table: "delivery_logs"
CREATE INDEX "deliverylog_provider_message_id" ON "delivery_logs" ("provider_message_id");
-- Modify "suppressions" table
ALTER TABLE "suppressions" ADD COLUMN "expires_at" timestamptz NULL;
