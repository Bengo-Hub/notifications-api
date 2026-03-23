-- Create "templates" table
CREATE TABLE "templates" ("id" uuid NOT NULL, "name" character varying NOT NULL, "channel" character varying NOT NULL, "category" character varying NOT NULL DEFAULT 'general', "tags" jsonb NULL, "file_path" character varying NOT NULL, "description" character varying NULL DEFAULT '', "variables" jsonb NULL, "mime_type" character varying NOT NULL DEFAULT 'text/plain', "is_active" boolean NOT NULL DEFAULT true, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "template_channel_category" to table: "templates"
CREATE INDEX "template_channel_category" ON "templates" ("channel", "category");
-- Create index "template_channel_name" to table: "templates"
CREATE UNIQUE INDEX "template_channel_name" ON "templates" ("channel", "name") WHERE (is_active = true);
