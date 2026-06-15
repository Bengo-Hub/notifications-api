-- Create "backups" table
CREATE TABLE "backups" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "name" character varying NOT NULL, "path" character varying NOT NULL, "size_bytes" bigint NOT NULL DEFAULT 0, "status" character varying NOT NULL DEFAULT 'completed', "created_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "backup_created_at" to table: "backups"
CREATE INDEX "backup_created_at" ON "backups" ("created_at");
-- Create index "backup_tenant_id_created_at" to table: "backups"
CREATE INDEX "backup_tenant_id_created_at" ON "backups" ("tenant_id", "created_at");
