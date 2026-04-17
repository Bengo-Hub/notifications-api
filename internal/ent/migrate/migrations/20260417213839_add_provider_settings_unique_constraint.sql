-- Drop index "providersetting_tenant_id_envi_4174d78779cdd3eabfc186faa75339aa" from table: "provider_settings"
DROP INDEX "providersetting_tenant_id_envi_4174d78779cdd3eabfc186faa75339aa";
-- Create index "providersetting_tenant_id_envi_4174d78779cdd3eabfc186faa75339aa" to table: "provider_settings"
CREATE UNIQUE INDEX "providersetting_tenant_id_envi_4174d78779cdd3eabfc186faa75339aa" ON "provider_settings" ("tenant_id", "environment", "provider_type", "provider_name", "key");
