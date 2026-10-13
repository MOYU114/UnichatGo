package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"unichatgo/internal/config"
	"unichatgo/internal/execution"
	"unichatgo/internal/storage"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config.json", "configuration containing the isolated/shared MySQL connection")
	root := flag.String("root", "", "required allowlisted source upload directory")
	sourceBase := flag.String("source-base", "", "required old backend working directory for relative stored paths")
	apply := flag.Bool("apply", false, "copy files and commit verified mapping; default is read-only dry run")
	id := flag.Int64("id", 0, "migrate only this legacy attachment ID; default scans all")
	timeout := flag.Duration("timeout", 10*time.Minute, "overall bounded migration timeout")
	flag.Parse()
	if *root == "" || *sourceBase == "" || *id < 0 || *timeout <= 0 {
		return fmt.Errorf("--root and --source-base are required; --id must be nonnegative and --timeout positive")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("configuration unavailable")
	}
	db, err := storage.Open("mysql", cfg)
	if err != nil {
		return fmt.Errorf("MySQL unavailable; verify connection configuration")
	}
	defer db.Close()
	repo := execution.NewRepository(db, execution.Limits{})
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err = repo.CheckSchema(ctx); err != nil {
		return fmt.Errorf("compatible async schema/write gate required; run approved schema migration separately")
	}
	migrator, err := execution.NewLegacyMigrator(repo, *root, *sourceBase)
	if err != nil {
		return fmt.Errorf("source root unavailable: %w", err)
	}
	defer migrator.Close()
	output := json.NewEncoder(os.Stdout)
	failures := 0
	copyOne := func(id int64) error {
		result, err := migrator.Copy(ctx, id, *apply)
		if err != nil {
			failures++
			// Do not print source paths, credentials, document bodies or SQL errors.
			code := "copy_or_verification_failed"
			if strings.HasPrefix(err.Error(), "legacy_") {
				code = err.Error()
			}
			return output.Encode(map[string]any{"legacy_id": id, "state": "error", "error": code})
		}
		if result.State == "expired" || result.State == "inactive" {
			failures++
		}
		return output.Encode(result)
	}
	if *id > 0 {
		if err := copyOne(*id); err != nil {
			return fmt.Errorf("report write failed; stop and rerun verification before continuing")
		}
	} else {
		var cursor int64
		for {
			files, err := repo.LegacyAttachments(ctx, cursor, 100)
			if err != nil {
				return fmt.Errorf("legacy metadata scan failed")
			}
			if len(files) == 0 {
				break
			}
			for _, file := range files {
				if err := copyOne(file.ID); err != nil {
					return fmt.Errorf("report write failed; stop and rerun verification before continuing")
				}
				cursor = file.ID
			}
			if ctx.Err() != nil {
				return fmt.Errorf("migration deadline reached; rerun safely after inspection")
			}
		}
	}
	if failures > 0 {
		return fmt.Errorf("%d legacy rows require explicit inspection; original files were not modified", failures)
	}
	return nil
}
