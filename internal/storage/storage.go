package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver with database/sql

	"werkstudent-radar/internal/model"
)

const schemaVersion = 1

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
	source TEXT NOT NULL,
	ref TEXT NOT NULL,
	title TEXT NOT NULL DEFAULT '',
	company TEXT NOT NULL DEFAULT '',
	location TEXT NOT NULL DEFAULT '',
	remote BOOLEAN NOT NULL DEFAULT 0,
	url TEXT NOT NULL DEFAULT '',
	published_at TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	first_seen TEXT NOT NULL,
	last_seen TEXT NOT NULL,
	PRIMARY KEY (source, ref)
);
CREATE INDEX IF NOT EXISTS jobs_first_seen ON jobs (first_seen);
`

type Storage struct {
	db *sql.DB
}

func Open(path string) (*Storage, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, errors.Join(fmt.Errorf("failed to ping database: %w", err), db.Close())
	}

	if _, err := db.Exec(schema); err != nil {
		return nil, errors.Join(fmt.Errorf("failed to create schema: %w", err), db.Close())
	}

	// PRAGMA does not support ? placeholders, so the value is formatted in.
	// Safe here: schemaVersion is our own constant, never user input.
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return nil, errors.Join(fmt.Errorf("failed to set schema version: %w", err), db.Close())
	}

	return &Storage{db: db}, nil
}

func (s *Storage) Close() error {
	return s.db.Close()
}

var ErrMissingKey = errors.New("missing key")

func (s *Storage) Save(ctx context.Context, jobs []model.Job, now time.Time) ([]model.Job, error) {
	ts := now.UTC().Format(time.RFC3339)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	// Rollback after a successful Commit only returns sql.ErrTxDone, so
	// its error is dropped on purpose; it only matters on early returns.
	defer func() { _ = tx.Rollback() }()

	var newJobs []model.Job
	for _, j := range jobs {
		if j.Source == "" || j.ReferenceNumber == "" {
			return nil, fmt.Errorf("job %q is missing source or reference number: %w", j.Title, ErrMissingKey)
		}
		result, err := tx.ExecContext(ctx, `
			INSERT INTO jobs (source, ref, title, company, location, remote, url, published_at, description, first_seen, last_seen)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (source, ref) DO NOTHING`,
			j.Source, j.ReferenceNumber, j.Title, j.Company, j.Location, j.Remote, j.URL, j.PublishedAt, j.Description, ts, ts)
		if err != nil {
			return nil, fmt.Errorf("failed to insert job %s/%s: %w", j.Source, j.ReferenceNumber, err)
		}

		n, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n == 1 {
			newJobs = append(newJobs, j)
			continue
		}

		// Known job: refresh what can change. description is left out on purpose:
		// search results never contain it, so updating it would wipe a fetched one.
		if _, err := tx.ExecContext(ctx, `
			UPDATE jobs SET title = ?, company = ?, location = ?, remote = ?, url = ?, published_at = ?, last_seen = ?
			WHERE source = ? AND ref = ?`,
			j.Title, j.Company, j.Location, j.Remote, j.URL, j.PublishedAt, ts, j.Source, j.ReferenceNumber); err != nil {
			return nil, fmt.Errorf("failed to update job %s/%s: %w", j.Source, j.ReferenceNumber, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return newJobs, nil
}
