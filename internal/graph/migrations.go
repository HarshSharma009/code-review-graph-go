package graph

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
)

const latestVersion = 6

func getSchemaVersion(db *sql.DB) (int, error) {
	var val string
	err := db.QueryRow("SELECT value FROM metadata WHERE key = 'schema_version'").Scan(&val)
	if err != nil {
		return 0, nil
	}
	var v int
	_, _ = fmt.Sscanf(val, "%d", &v)
	if v == 0 {
		return 1, nil
	}
	return v, nil
}

func setSchemaVersion(tx *sql.Tx, version int) error {
	_, err := tx.Exec(
		"INSERT OR REPLACE INTO metadata (key, value) VALUES ('schema_version', ?)",
		fmt.Sprintf("%d", version),
	)
	return err
}

func hasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table)) //nolint:gosec
	if err != nil {
		return false, fmt.Errorf("pragma table_info(%s): %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return false, fmt.Errorf("scanning column info: %w", err)
		}
		if strings.EqualFold(name, column) {
			return true, nil
		}
	}
	return false, rows.Err()
}

func tableExists(db *sql.DB, table string) (bool, error) {
	var count int
	err := db.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE type IN ('table', 'view') AND name = ?",
		table,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("checking table existence: %w", err)
	}
	return count > 0, nil
}

type migration struct {
	version int
	fn      func(db *sql.DB, tx *sql.Tx) error
}

var migrations = []migration{
	{2, migrateV2},
	{3, migrateV3},
	{4, migrateV4},
	{5, migrateV5},
	{6, migrateV6},
}

func migrateV2(db *sql.DB, tx *sql.Tx) error {
	has, err := hasColumn(db, "nodes", "signature")
	if err != nil {
		return err
	}
	if !has {
		if _, err := tx.Exec("ALTER TABLE nodes ADD COLUMN signature TEXT"); err != nil {
			return fmt.Errorf("adding signature column: %w", err)
		}
		slog.Info("migration v2: added signature column to nodes")
	}
	return nil
}

func migrateV3(_ *sql.DB, tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS flows (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			entry_point_id INTEGER NOT NULL,
			depth INTEGER NOT NULL,
			node_count INTEGER NOT NULL,
			file_count INTEGER NOT NULL,
			criticality REAL NOT NULL DEFAULT 0.0,
			path_json TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS flow_memberships (
			flow_id INTEGER NOT NULL,
			node_id INTEGER NOT NULL,
			position INTEGER NOT NULL,
			PRIMARY KEY (flow_id, node_id)
		)`,
		"CREATE INDEX IF NOT EXISTS idx_flows_criticality ON flows(criticality DESC)",
		"CREATE INDEX IF NOT EXISTS idx_flows_entry ON flows(entry_point_id)",
		"CREATE INDEX IF NOT EXISTS idx_flow_memberships_node ON flow_memberships(node_id)",
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("migration v3: %w", err)
		}
	}
	slog.Info("migration v3: created flows and flow_memberships tables")
	return nil
}

func migrateV4(db *sql.DB, tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS communities (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		level INTEGER NOT NULL DEFAULT 0,
		parent_id INTEGER,
		cohesion REAL NOT NULL DEFAULT 0.0,
		size INTEGER NOT NULL DEFAULT 0,
		dominant_language TEXT,
		description TEXT,
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		return fmt.Errorf("creating communities table: %w", err)
	}

	has, err := hasColumn(db, "nodes", "community_id")
	if err != nil {
		return err
	}
	if !has {
		if _, err := tx.Exec("ALTER TABLE nodes ADD COLUMN community_id INTEGER"); err != nil {
			return fmt.Errorf("adding community_id column: %w", err)
		}
		slog.Info("migration v4: added community_id column to nodes")
	}

	idxs := []string{
		"CREATE INDEX IF NOT EXISTS idx_nodes_community ON nodes(community_id)",
		"CREATE INDEX IF NOT EXISTS idx_communities_parent ON communities(parent_id)",
		"CREATE INDEX IF NOT EXISTS idx_communities_cohesion ON communities(cohesion DESC)",
	}
	for _, s := range idxs {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("migration v4 index: %w", err)
		}
	}
	slog.Info("migration v4: created communities table")
	return nil
}

func migrateV5(db *sql.DB, _ *sql.Tx) error {
	exists, err := tableExists(db, "nodes_fts")
	if err != nil {
		return err
	}
	if !exists {
		if _, err := db.Exec(`CREATE VIRTUAL TABLE nodes_fts USING fts5(
			name, qualified_name, file_path, signature,
			content='nodes', content_rowid='rowid',
			tokenize='porter unicode61'
		)`); err != nil {
			return fmt.Errorf("creating FTS5 table: %w", err)
		}
		slog.Info("migration v5: created nodes_fts FTS5 virtual table")
	}
	return nil
}

func migrateV6(_ *sql.DB, tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS community_summaries (
			community_id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			purpose TEXT DEFAULT '',
			key_symbols TEXT DEFAULT '[]',
			risk TEXT DEFAULT 'unknown',
			size INTEGER DEFAULT 0,
			dominant_language TEXT DEFAULT '',
			FOREIGN KEY (community_id) REFERENCES communities(id)
		)`,
		`CREATE TABLE IF NOT EXISTS flow_snapshots (
			flow_id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			entry_point TEXT NOT NULL,
			critical_path TEXT DEFAULT '[]',
			criticality REAL DEFAULT 0.0,
			node_count INTEGER DEFAULT 0,
			file_count INTEGER DEFAULT 0,
			FOREIGN KEY (flow_id) REFERENCES flows(id)
		)`,
		`CREATE TABLE IF NOT EXISTS risk_index (
			node_id INTEGER PRIMARY KEY,
			qualified_name TEXT NOT NULL,
			risk_score REAL DEFAULT 0.0,
			caller_count INTEGER DEFAULT 0,
			test_coverage TEXT DEFAULT 'unknown',
			security_relevant INTEGER DEFAULT 0,
			last_computed TEXT DEFAULT '',
			FOREIGN KEY (node_id) REFERENCES nodes(id)
		)`,
		"CREATE INDEX IF NOT EXISTS idx_risk_index_score ON risk_index(risk_score DESC)",
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("migration v6: %w", err)
		}
	}
	slog.Info("migration v6: created summary tables")
	return nil
}

func runMigrations(db *sql.DB) error {
	current, err := getSchemaVersion(db)
	if err != nil {
		return fmt.Errorf("getting schema version: %w", err)
	}
	if current >= latestVersion {
		return nil
	}

	slog.Info("running migrations", "from", current, "to", latestVersion)

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		slog.Info("running migration", "version", m.version)

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("beginning migration v%d tx: %w", m.version, err)
		}

		if err := m.fn(db, tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration v%d failed: %w", m.version, err)
		}

		if err := setSchemaVersion(tx, m.version); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("setting schema version to %d: %w", m.version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing migration v%d: %w", m.version, err)
		}
	}

	slog.Info("migrations complete", "version", latestVersion)
	return nil
}
