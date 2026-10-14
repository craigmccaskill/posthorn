package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/craigmccaskill/posthorn/spam"
	"github.com/craigmccaskill/posthorn/storage"
)

// runSuppressions implements `posthorn suppressions` (FR87, ADR-23) —
// the management and GDPR-erasure surface for the suppression list.
// Operates directly on the configured storage file; there is no HTTP
// admin API by design. Run it against a live instance's file freely:
// SQLite WAL handles a concurrent reader/writer from the same host.
func runSuppressions(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("suppressions: subcommand required: list | add <email> | remove <email>")
	}
	sub := args[0]
	rest := args[1:]

	// add/remove take the email as the first positional after the verb.
	var email string
	if sub == "add" || sub == "remove" {
		if len(rest) < 1 || len(rest[0]) == 0 || rest[0][0] == '-' {
			return fmt.Errorf("suppressions %s: email argument required", sub)
		}
		email = rest[0]
		rest = rest[1:]
	}

	fs := flag.NewFlagSet("suppressions "+sub, flag.ContinueOnError)
	configPath := fs.String("config", "/etc/posthorn/config.toml", "path to TOML config file (ignored when POSTHORN_CONFIG is set)")
	reason := fs.String("reason", storage.ReasonManual, "suppression reason (add only)")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	st, err := openConfiguredStore(*configPath)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	switch sub {
	case "list":
		rows, err := st.ListSuppressions(10000)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			fmt.Println("no suppressions")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "EMAIL\tREASON\tSOURCE\tSINCE")
		for _, r := range rows {
			src := r.SourceEndpoint
			if src == "" {
				src = "-"
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Email, r.Reason, src, r.CreatedAt.Format(time.RFC3339))
		}
		return w.Flush()

	case "add":
		if err := st.AddSuppression(email, *reason, "", time.Now()); err != nil {
			return err
		}
		fmt.Printf("suppressed %s (%s)\n", email, *reason)
		return nil

	case "remove":
		n, err := st.RemoveSuppression(email)
		if err != nil {
			return err
		}
		if n == 0 {
			fmt.Printf("%s was not suppressed\n", email)
		} else {
			fmt.Printf("removed %s (%d entr%s)\n", email, n, plural(n, "y", "ies"))
		}
		return nil

	default:
		return fmt.Errorf("suppressions: unknown subcommand %q (want list | add | remove)", sub)
	}
}

// openConfiguredStore loads the config and opens its storage file.
func openConfiguredStore(configPath string) (*storage.Store, error) {
	cfg, configSource, err := loadConfig(configPath)
	if err != nil {
		return nil, err
	}
	if cfg.Storage == nil {
		return nil, fmt.Errorf("no [storage] block in %s — suppressions need the storage layer", configSource)
	}
	maxSize, err := spam.ParseSize(cfg.Storage.EffectiveMaxSize())
	if err != nil {
		return nil, fmt.Errorf("storage.max_size: %w", err)
	}
	return storage.Open(storage.Config{
		Path:      cfg.Storage.Path,
		InMemory:  cfg.Storage.InMemory,
		Retention: cfg.Storage.EffectiveRetention(),
		MaxSize:   maxSize,
	})
}

func plural(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
