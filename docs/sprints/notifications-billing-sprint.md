# Sprint Doc: Credit-Based Notifications (SMS/WhatsApp)

## Overview
Implement a credit-based billing system for SMS and WhatsApp notifications. Tenants must purchase credits at platform-set rates to send messages. Email remains free.

## Objectives
1.  **Backend (Subscriptions-API)**:
    *   Track credit balances per tenant.
    *   Provide endpoints for purchasing credits (via Treasury).
    *   Expose real-time balance check for Notification-API.
2.  **Notification-API**:
    *   Integrate balance check before sending SMS/WhatsApp.
    *   Deduct credits upon successful delivery.
3.  **UI (Subscriptions-UI/Auth-UI)**:
    *   Platform Admin: Set SMS/WhatsApp credit rates.
    *   Tenant Admin: View balance, purchase credits, and view usage history.

## Detailed Work Items

### Phase 1: Storage & Models
- `TenantCredit` table:
    - `tenant_id` (UUID)
    - `credit_type` (ENUM: SMS, WHATSAPP)
    - `balance` (DECIMAL)
    - `rate` (DECIMAL) - Current rate at which credits were bought or platform rate.
- `CreditTransaction` table:
    - `tenant_id`, `amount`, `type` (PURCHASE, USAGE, REFUND), `metadata`.

### Phase 2: Credit Logic
- **Pre-send Guard**: `IF balance < required_credits THEN REJECT`.
- **Atomic Deduction**: Ensure deduction happens in Tx or via reliable event.
- **Low Balance Alerts**: Notify tenant when balance < 20%.

### Phase 3: Platform Admin UI
- Interface to set "Price per SMS Credit" (e.g., 1 Credit = 1.0 KES, Platform sells at 1.5 KES).
- Tenant-specific rate overrides for high-volume users.

## Progress Tracking
- [x] Schema definition — `TenantCredit` + `CreditTransaction` Ent schemas, Atlas migration applied
- [x] Integration with Notification Service — pre-send credit guard in `Enqueue` handler (HTTP 402 on zero balance); `billing.Service` wired into notification handler
- [x] Purchase workflow (Treasury redirect) — `POST /api/v1/billing/initiate` creates treasury payment intent; `treasury_consumer` handles `payment.succeeded` with `reference_type=topup` to call `TopUpCredits`; `TreasuryPaymentModal` wired in credits page
- [x] Balance dashboard in UI — credits page uses `useCreditBalance` + `useCreditTransactions` hooks backed by `GET /api/v1/billing/balance` and `GET /api/v1/billing/transactions`

## Completed: 2026-05-22
