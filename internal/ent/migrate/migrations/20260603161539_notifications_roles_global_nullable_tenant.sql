-- Modify "notification_roles" table
ALTER TABLE "notification_roles" ALTER COLUMN "tenant_id" DROP NOT NULL;
-- Create index "notificationrole_role_code" to table: "notification_roles"
CREATE INDEX "notificationrole_role_code" ON "notification_roles" ("role_code");
