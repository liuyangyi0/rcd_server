// Administrative, one-time migration. Stop the manager through Semaphore first;
// this tool never stops/restarts services or modifies the Agent/lower configs.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"rcd_manager/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	database := flag.String("database", "", "absolute manager.db path")
	backup := flag.String("backup", "", "new absolute backup filename; never overwritten")
	offline := flag.Bool("offline", false, "confirm target manager is stopped and controls are paused")
	flag.Parse()
	if !*offline || flag.NArg() != 0 || !filepath.IsAbs(*database) || !filepath.IsAbs(*backup) || filepath.Clean(*database) == filepath.Clean(*backup) {
		return fmt.Errorf("requires --offline --database ABSOLUTE_PATH --backup NEW_ABSOLUTE_PATH; stop manager first")
	}
	info, err := os.Lstat(*database)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("database must be a regular file")
	}
	if _, err = os.Lstat(*backup); !os.IsNotExist(err) {
		return fmt.Errorf("backup already exists or cannot be checked")
	}
	ctx := context.Background()
	db, err := sql.Open("sqlite", *database)
	if err != nil {
		return err
	}
	// Back up before opening a writable repository. VACUUM INTO includes WAL
	// data; this one-time ID migration does not add a database schema version.
	_, err = db.ExecContext(ctx, `VACUUM INTO '`+strings.ReplaceAll(*backup, "'", "''")+`'`)
	closeErr := db.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Chmod(*backup, 0o600); err != nil {
		return err
	}
	repo, err := store.Open(ctx, *database, store.PassthroughProtector{})
	if err != nil {
		return err
	}
	defer repo.Close()
	result, err := repo.NormalizeLegacySourceIDs(ctx)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
