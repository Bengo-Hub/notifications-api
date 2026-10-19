-- SMS and WhatsApp billing moved to subscriptions-api (one prepaid credits ledger). This service no
-- longer keeps wallets, platform billing rates or WhatsApp plans and subscriptions.
DROP TABLE IF EXISTS "credit_transactions";
DROP TABLE IF EXISTS "tenant_credits";
DROP TABLE IF EXISTS "platform_billings";
DROP TABLE IF EXISTS "tenant_whats_app_subscriptions";
DROP TABLE IF EXISTS "whats_app_plans";
