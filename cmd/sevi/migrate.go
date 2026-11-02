package main

import (
	"context"
	"fmt"
	"os"

	"sevi/internal/secrets"
	"sevi/internal/store"
)

// migrateCmd applies every pending migration to the database and exits. The
// connection string comes from the DATABASE_URL environment variable if set (how
// the deploy runs it, before any secret exists to read), else from the secrets.
// Migrations are safe to run again: applied ones are recorded and skipped.
func (a *app) migrateCmd(ctx context.Context) int {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		_, sec, err := secrets.Open(ctx, a.cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		a.Sec = sec
		if url, err = a.Secret("database", "DATABASE_URL"); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
	}
	st, err := store.Open(ctx, url, a.Log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer st.Close()
	fmt.Println("database schema is up to date")
	return 0
}
