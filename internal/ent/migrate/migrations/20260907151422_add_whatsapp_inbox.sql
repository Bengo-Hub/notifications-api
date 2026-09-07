-- Create "whats_app_conversations" table
CREATE TABLE "whats_app_conversations" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "phone_number_id" character varying NOT NULL, "customer_wa_id" character varying NOT NULL, "customer_name" character varying NULL, "last_message_at" timestamptz NOT NULL, "last_message_preview" character varying NULL, "last_inbound_at" timestamptz NULL, "unread_count" bigint NOT NULL DEFAULT 0, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "whatsappconversation_tenant_id_last_message_at" to table: "whats_app_conversations"
CREATE INDEX "whatsappconversation_tenant_id_last_message_at" ON "whats_app_conversations" ("tenant_id", "last_message_at");
-- Create index "whatsappconversation_tenant_id_phone_number_id_customer_wa_id" to table: "whats_app_conversations"
CREATE UNIQUE INDEX "whatsappconversation_tenant_id_phone_number_id_customer_wa_id" ON "whats_app_conversations" ("tenant_id", "phone_number_id", "customer_wa_id");
-- Create "whats_app_messages" table
CREATE TABLE "whats_app_messages" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "direction" character varying NOT NULL, "message_type" character varying NOT NULL DEFAULT 'text', "body" text NOT NULL, "wa_message_id" character varying NULL, "status" character varying NOT NULL DEFAULT 'received', "sent_by_user_id" uuid NULL, "created_at" timestamptz NOT NULL, "conversation_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "whats_app_messages_whats_app_conversations_conversation" FOREIGN KEY ("conversation_id") REFERENCES "whats_app_conversations" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "whatsappmessage_conversation_id_created_at" to table: "whats_app_messages"
CREATE INDEX "whatsappmessage_conversation_id_created_at" ON "whats_app_messages" ("conversation_id", "created_at");
-- Create index "whatsappmessage_tenant_id" to table: "whats_app_messages"
CREATE INDEX "whatsappmessage_tenant_id" ON "whats_app_messages" ("tenant_id");
-- Create index "whatsappmessage_wa_message_id" to table: "whats_app_messages"
CREATE UNIQUE INDEX "whatsappmessage_wa_message_id" ON "whats_app_messages" ("wa_message_id");
