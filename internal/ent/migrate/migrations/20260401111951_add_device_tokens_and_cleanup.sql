-- Modify "tenants" table
ALTER TABLE "tenants" ADD COLUMN "sync_status" character varying NOT NULL DEFAULT 'synced', ADD COLUMN "last_sync_at" timestamptz NULL;
-- Create "device_tokens" table
CREATE TABLE "device_tokens" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "user_id" uuid NOT NULL, "token" text NOT NULL, "platform" character varying NOT NULL DEFAULT 'android', "provider" character varying NOT NULL DEFAULT 'fcm', "is_active" boolean NOT NULL DEFAULT true, "last_used_at" timestamptz NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "devicetoken_is_active" to table: "device_tokens"
CREATE INDEX "devicetoken_is_active" ON "device_tokens" ("is_active");
-- Create index "devicetoken_tenant_id_user_id" to table: "device_tokens"
CREATE INDEX "devicetoken_tenant_id_user_id" ON "device_tokens" ("tenant_id", "user_id");
-- Create index "devicetoken_token" to table: "device_tokens"
CREATE UNIQUE INDEX "devicetoken_token" ON "device_tokens" ("token");
