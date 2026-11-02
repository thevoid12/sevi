package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"

	"github.com/pulumi/pulumi-command/sdk/go/command/local"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const migrationsDir = "../internal/store/sql/migrations"

// runMigrations applies the database schema by running `sevi migrate` against the
// Neon database. The command is idempotent (applied migrations are skipped) and
// re-runs when any migration file changes. The app also migrates when it starts,
// under a database lock, so the two never conflict; doing it here means the schema
// exists, and a bad migration fails the deploy, before Cloud Run starts.
func runMigrations(ctx *pulumi.Context, databaseURL pulumi.StringInput) (*local.Command, error) {
	digest, err := migrationsDigest(migrationsDir)
	if err != nil {
		return nil, err
	}
	return local.NewCommand(ctx, "db-migrate", &local.CommandArgs{
		Dir:         pulumi.String(".."),
		Create:      pulumi.String("go run ./cmd/sevi -config config.prod.json migrate"),
		Update:      pulumi.String("go run ./cmd/sevi -config config.prod.json migrate"),
		Environment: pulumi.StringMap{"DATABASE_URL": databaseURL},
		Triggers:    pulumi.Array{pulumi.String(digest)},
	})
}

// migrationsDigest hashes the migration files so a change triggers a re-run.
func migrationsDigest(dir string) (string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	h := sha256.New()
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		h.Write([]byte(filepath.Base(f)))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
