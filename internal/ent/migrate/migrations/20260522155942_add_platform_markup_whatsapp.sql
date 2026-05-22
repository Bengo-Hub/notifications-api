-- Modify "credit_transactions" table
ALTER TABLE "credit_transactions" ADD COLUMN "provider_cost" double precision NOT NULL DEFAULT 0, ADD COLUMN "platform_fee" double precision NOT NULL DEFAULT 0;
-- Modify "platform_billings" table
ALTER TABLE "platform_billings" ADD COLUMN "provider_cost_per_sms" double precision NOT NULL DEFAULT 0.5, ADD COLUMN "provider_cost_per_whatsapp" double precision NOT NULL DEFAULT 0.8, ADD COLUMN "min_markup_percentage" double precision NOT NULL DEFAULT 40;
-- Create "whats_app_plans" table
CREATE TABLE "whats_app_plans" ("id" uuid NOT NULL, "name" character varying NOT NULL, "slug" character varying NOT NULL, "price_monthly" double precision NOT NULL, "provider_cost" double precision NOT NULL DEFAULT 0, "messages_per_month" bigint NOT NULL DEFAULT 0, "is_active" boolean NOT NULL DEFAULT true, "created_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "whats_app_plans_slug_key" to table: "whats_app_plans"
CREATE UNIQUE INDEX "whats_app_plans_slug_key" ON "whats_app_plans" ("slug");
-- Create "tenant_whats_app_subscriptions" table
CREATE TABLE "tenant_whats_app_subscriptions" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "status" character varying NOT NULL DEFAULT 'trial', "started_at" timestamptz NOT NULL, "expires_at" timestamptz NOT NULL, "payment_reference" character varying NULL, "auto_renew" boolean NOT NULL DEFAULT true, "messages_used" bigint NOT NULL DEFAULT 0, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "plan_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "tenant_whats_app_subscriptions_whats_app_plans_plan" FOREIGN KEY ("plan_id") REFERENCES "whats_app_plans" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "tenantwhatsappsubscription_tenant_id" to table: "tenant_whats_app_subscriptions"
CREATE INDEX "tenantwhatsappsubscription_tenant_id" ON "tenant_whats_app_subscriptions" ("tenant_id");
-- Create index "tenantwhatsappsubscription_tenant_id_status" to table: "tenant_whats_app_subscriptions"
CREATE INDEX "tenantwhatsappsubscription_tenant_id_status" ON "tenant_whats_app_subscriptions" ("tenant_id", "status");
