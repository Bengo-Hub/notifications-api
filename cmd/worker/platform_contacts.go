package main

// Codevertex mailboxes the worker names on platform mail (the codevertex tenant and
// platform-scope messages). Tenant-scope mail uses the tenant's own support contact instead.
const (
	// platformInfoEmail is the Reply-To on platform-scope mail that sets none (owner decision, 18 August 2026).
	platformInfoEmail = "info@codevertexafrica.com"
	// platformSupportEmail answers the platform's own tickets when the codevertex tenant has no
	// support contact set (support routing, owner decisions 12 November 2026).
	platformSupportEmail = "support@codevertexafrica.com"
	// platformBillingEmail handles subscription, invoice and payment questions.
	platformBillingEmail = "billing@codevertexafrica.com"
)
