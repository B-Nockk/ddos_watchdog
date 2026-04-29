package main

import (
	"database/sql"
	"log"
	"time"

	_ "modernc.org/sqlite"
)

// OffenseStore is the persistence layer for IP offense history.
// It lives entirely within the Blocker boundary — no other component
// reads from or writes to it directly.
//
// Schema:
//
//	offense_history
//	  ip          TEXT PRIMARY KEY
//	  count       INTEGER   -- total lifetime offense count for this IP
//	  last_seen   DATETIME  -- when the most recent offense was recorded
//
// The store is intentionally minimal. It answers one question:
// "given this IP, what is its current offense count, accounting for decay?"
type OffenseStore struct {
	db                 *sql.DB
	offenseMemoryHours int
}

// NewOffenseStore opens (or creates) the SQLite database at the given path
// and ensures the schema exists.
func NewOffenseStore(dbPath string, offenseMemoryHours int) (*OffenseStore, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}

	// Single-writer workload — WAL mode gives better read concurrency
	// and is safer on crash than the default journal mode.
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		return nil, err
	}

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS offense_history (
			ip        TEXT    PRIMARY KEY,
			count     INTEGER NOT NULL DEFAULT 0,
			last_seen DATETIME NOT NULL
		)
	`); err != nil {
		return nil, err
	}

	log.Printf("[store] opened offense history db at %s (memory window: %dh)", dbPath, offenseMemoryHours)

	return &OffenseStore{
		db:                 db,
		offenseMemoryHours: offenseMemoryHours,
	}, nil
}

// OffenseCount returns the current offense count for an IP.
// If the IP's last offense was more than offenseMemoryHours ago,
// the record is treated as expired and 0 is returned (fresh start).
func (s *OffenseStore) OffenseCount(ip string) int {
	row := s.db.QueryRow(
		`SELECT count, last_seen FROM offense_history WHERE ip = ?`, ip,
	)

	var count int
	var lastSeenStr string

	if err := row.Scan(&count, &lastSeenStr); err != nil {
		// No record — first time we've seen this IP.
		return 0
	}

	lastSeen, err := time.Parse(time.RFC3339, lastSeenStr)
	if err != nil {
		return 0
	}

	// Apply decay: if the last offense is outside the memory window,
	// this IP has been clean long enough to start fresh.
	cutoff := time.Now().Add(-time.Duration(s.offenseMemoryHours) * time.Hour)
	if lastSeen.Before(cutoff) {
		log.Printf("[store] %s offense history expired (last seen %s) — resetting", ip, lastSeen.Format(time.RFC3339))
		return 0
	}

	return count
}

// RecordOffense increments the offense count for an IP and updates last_seen.
// Called by Blocker.Ban() after the ban record is created.
func (s *OffenseStore) RecordOffense(ip string, newCount int) {
	_, err := s.db.Exec(`
		INSERT INTO offense_history (ip, count, last_seen)
		VALUES (?, ?, ?)
		ON CONFLICT(ip) DO UPDATE SET
			count     = excluded.count,
			last_seen = excluded.last_seen
	`, ip, newCount, time.Now().UTC().Format(time.RFC3339))

	if err != nil {
		log.Printf("[store] RecordOffense %s: %v", ip, err)
	}
}

// Close closes the underlying database connection.
func (s *OffenseStore) Close() error {
	return s.db.Close()
}
