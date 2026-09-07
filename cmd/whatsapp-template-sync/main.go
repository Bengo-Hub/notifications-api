// Command whatsapp-template-sync is a thin CLI wrapper over internal/whatsapp/templatesync — see
// that package for the actual sync logic (shared with the platform admin HTTP endpoint / UI).
//
// Usage:
//
//	WHATSAPP_WABA_ID=... WHATSAPP_ACCESS_TOKEN=... go run ./cmd/whatsapp-template-sync -dry-run
//	WHATSAPP_WABA_ID=... WHATSAPP_ACCESS_TOKEN=... go run ./cmd/whatsapp-template-sync
//	WHATSAPP_WABA_ID=... WHATSAPP_ACCESS_TOKEN=... go run ./cmd/whatsapp-template-sync -only=finance_
//
// -dry-run prints exactly what would be created (and what's already present, skipped) without
// calling Meta's create endpoint at all — always run this first.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/bengobox/notifications-api/internal/whatsapp/templatesync"
)

func main() {
	dryRun := flag.Bool("dry-run", false, "print what would be created without calling Meta")
	only := flag.String("only", "", "comma-separated name prefixes to limit this run to, e.g. -only=finance_ (submits finance_payment_success, finance_invoice_sent, ...). Empty means every template in templates.json.")
	flag.Parse()

	wabaID := os.Getenv("WHATSAPP_WABA_ID")
	token := os.Getenv("WHATSAPP_ACCESS_TOKEN")
	if wabaID == "" || token == "" {
		fmt.Fprintln(os.Stderr, "WHATSAPP_WABA_ID and WHATSAPP_ACCESS_TOKEN must both be set")
		os.Exit(1)
	}

	defs, err := templatesync.LoadManifest()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *only != "" {
		defs = templatesync.FilterByPrefix(defs, strings.Split(*only, ","))
	}
	fmt.Printf("loaded %d template definitions\n\n", len(defs))

	syncer := templatesync.NewSyncer(wabaID, token)
	results, err := syncer.Run(context.Background(), defs, *dryRun)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	created, skipped, failed := 0, 0, 0
	for _, r := range results {
		switch r.Outcome {
		case templatesync.OutcomeSkipped:
			fmt.Printf("SKIP  %-38s %s\n", r.Name, r.Detail)
			skipped++
		case templatesync.OutcomeFailed:
			fmt.Printf("FAIL  %-38s %s\n", r.Name, r.Detail)
			failed++
		case templatesync.OutcomeCreated:
			if r.DryRun {
				fmt.Printf("WOULD CREATE  %-30s category=%s\n", r.Name, r.Category)
			} else {
				fmt.Printf("CREATED  %-35s submitted for Meta review\n", r.Name)
				created++
			}
		}
	}

	fmt.Println()
	if *dryRun {
		fmt.Println("dry run — nothing was sent to Meta")
		return
	}
	fmt.Printf("done: %d created, %d skipped (already existed), %d failed\n", created, skipped, failed)
	if failed > 0 {
		os.Exit(1)
	}
}
