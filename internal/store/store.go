package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type BalanceSample struct {
	Timestamp       int64   `json:"timestamp"`
	AvailableAmount float64 `json:"available_amount"`
}

type Metrics struct {
	TrafficGB        float64 `json:"traffic_gb"`
	TrafficThreshold float64 `json:"traffic_threshold_gb"`
	DailyRemainingGB float64 `json:"daily_remaining_gb"`
}

type ECSState struct {
	LastStatus                 string
	LastStartupTimestamp       int64
	ScheduledStopActive        bool
	LastScheduledStopTimestamp int64
	LastUnexpectedTimestamp    int64
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	}
	for _, statement := range pragmas {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, err
		}
	}
	store := &Store{db: db}
	if err := store.initializeSchema(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) SaveLastResult(result map[string]any) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO runtime_state(key, value) VALUES('last_result', ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, string(data))
	return err
}

func (s *Store) LastResult(fallback map[string]any) (map[string]any, error) {
	var raw string
	err := s.db.QueryRow("SELECT value FROM runtime_state WHERE key='last_result'").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return cloneMap(fallback), nil
	}
	if err != nil {
		return nil, err
	}
	result := make(map[string]any)
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return cloneMap(fallback), nil
	}
	return result, nil
}

func (s *Store) SaveMetrics(metrics Metrics) error {
	_, err := s.db.Exec(`INSERT INTO latest_metrics(id, traffic_gb, traffic_threshold_gb, daily_remaining_gb)
		VALUES(1, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET traffic_gb=excluded.traffic_gb,
		traffic_threshold_gb=excluded.traffic_threshold_gb,
		daily_remaining_gb=excluded.daily_remaining_gb`,
		metrics.TrafficGB, metrics.TrafficThreshold, metrics.DailyRemainingGB)
	return err
}

func (s *Store) Metrics() (Metrics, bool, error) {
	var metrics Metrics
	err := s.db.QueryRow(`SELECT traffic_gb, traffic_threshold_gb, daily_remaining_gb
		FROM latest_metrics WHERE id=1`).Scan(
		&metrics.TrafficGB, &metrics.TrafficThreshold, &metrics.DailyRemainingGB)
	if errors.Is(err, sql.ErrNoRows) {
		return Metrics{}, false, nil
	}
	return metrics, err == nil, err
}

func (s *Store) AddBalance(timestamp int64, available float64) error {
	_, err := s.db.Exec("INSERT INTO balance_samples(timestamp, available_amount) VALUES(?, ?)", timestamp, available)
	return err
}

func (s *Store) AddTraffic(timestamp int64, trafficGB float64) error {
	_, err := s.db.Exec("INSERT INTO traffic_samples(timestamp, traffic_gb) VALUES(?, ?)", timestamp, trafficGB)
	return err
}

// TodayTraffic returns the traffic used since local midnight. If no earlier
// baseline exists, traffic is calculated from zero.
func (s *Store) TodayTraffic(now time.Time) (float64, bool, error) {
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	var baseline, latest float64
	err := s.db.QueryRow(`SELECT traffic_gb FROM traffic_samples WHERE timestamp >= ?
		ORDER BY timestamp DESC, id DESC LIMIT 1`, start).Scan(&latest)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if now.Day() == 1 {
		return latest, true, nil // CDT's cumulative traffic counter resets each month.
	}
	err = s.db.QueryRow(`SELECT traffic_gb FROM traffic_samples WHERE timestamp < ?
		ORDER BY timestamp DESC, id DESC LIMIT 1`, start).Scan(&baseline)
	if errors.Is(err, sql.ErrNoRows) {
		return latest, true, nil
	}
	if err != nil {
		return 0, false, err
	}
	return latest - baseline, true, nil
}

func (s *Store) BalanceSamples() ([]BalanceSample, error) {
	rows, err := s.db.Query("SELECT timestamp, available_amount FROM balance_samples ORDER BY timestamp, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]BalanceSample, 0)
	for rows.Next() {
		var sample BalanceSample
		if err := rows.Scan(&sample.Timestamp, &sample.AvailableAmount); err != nil {
			return nil, err
		}
		result = append(result, sample)
	}
	return result, rows.Err()
}

func (s *Store) ECSState() (ECSState, error) {
	var state ECSState
	var active int
	err := s.db.QueryRow(`SELECT last_status, last_startup_timestamp, scheduled_stop_active,
		last_scheduled_stop_timestamp, last_unexpected_stop_timestamp
		FROM ecs_state WHERE id=1`).Scan(
		&state.LastStatus, &state.LastStartupTimestamp, &active,
		&state.LastScheduledStopTimestamp, &state.LastUnexpectedTimestamp,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ECSState{}, nil
	}
	state.ScheduledStopActive = active != 0
	return state, err
}

func (s *Store) RecordECSState(state ECSState, scheduledEvent, unexpectedEvent bool, now int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO ecs_state(id, last_status, last_startup_timestamp,
		scheduled_stop_active, last_scheduled_stop_timestamp, last_unexpected_stop_timestamp)
		VALUES(1, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET last_status=excluded.last_status,
		last_startup_timestamp=excluded.last_startup_timestamp,
		scheduled_stop_active=excluded.scheduled_stop_active,
		last_scheduled_stop_timestamp=excluded.last_scheduled_stop_timestamp,
		last_unexpected_stop_timestamp=excluded.last_unexpected_stop_timestamp`,
		state.LastStatus, state.LastStartupTimestamp, boolInt(state.ScheduledStopActive),
		state.LastScheduledStopTimestamp, state.LastUnexpectedTimestamp); err != nil {
		return err
	}
	if scheduledEvent {
		if _, err := tx.Exec("INSERT INTO stop_events(kind, timestamp) VALUES('scheduled', ?)", now); err != nil {
			return err
		}
	}
	if unexpectedEvent {
		if _, err := tx.Exec("INSERT INTO stop_events(kind, timestamp) VALUES('unexpected', ?)", now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) StopCounts(cutoff int64) (scheduled, unexpected int, err error) {
	rows, err := s.db.Query(`SELECT kind, COUNT(*) FROM stop_events WHERE timestamp >= ? GROUP BY kind`, cutoff)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var count int
		if err := rows.Scan(&kind, &count); err != nil {
			return 0, 0, err
		}
		if kind == "scheduled" {
			scheduled = count
		} else if kind == "unexpected" {
			unexpected = count
		}
	}
	return scheduled, unexpected, rows.Err()
}

func (s *Store) Prune(balanceCutoff, eventCutoff int64) error {
	_, err := s.db.Exec(`DELETE FROM balance_samples WHERE timestamp < ?;
		DELETE FROM traffic_samples WHERE timestamp < ?;
		DELETE FROM stop_events WHERE timestamp < ?;`, balanceCutoff, balanceCutoff, eventCutoff)
	return err
}

func (s *Store) initializeSchema() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS runtime_state (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS latest_metrics (
			id INTEGER PRIMARY KEY CHECK(id=1),
			traffic_gb REAL NOT NULL,
			traffic_threshold_gb REAL NOT NULL,
			daily_remaining_gb REAL NOT NULL
		);
	CREATE TABLE IF NOT EXISTS balance_samples (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp INTEGER NOT NULL,
			available_amount REAL NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_balance_timestamp ON balance_samples(timestamp);
	CREATE TABLE IF NOT EXISTS traffic_samples (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp INTEGER NOT NULL,
		traffic_gb REAL NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_traffic_timestamp ON traffic_samples(timestamp);
		CREATE TABLE IF NOT EXISTS ecs_state (
			id INTEGER PRIMARY KEY CHECK(id=1),
			last_status TEXT NOT NULL DEFAULT '',
			last_startup_timestamp INTEGER NOT NULL DEFAULT 0,
			scheduled_stop_active INTEGER NOT NULL DEFAULT 0,
			last_scheduled_stop_timestamp INTEGER NOT NULL DEFAULT 0,
			last_unexpected_stop_timestamp INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE IF NOT EXISTS stop_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			kind TEXT NOT NULL CHECK(kind IN ('scheduled', 'unexpected')),
			timestamp INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_stop_event_timestamp ON stop_events(timestamp);
	`)
	return err
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func cloneMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func DBPath() string {
	if path := os.Getenv("CDT_DB_FILE"); path != "" {
		return path
	}
	return filepath.Join("data", "cdtalive.db")
}

func PreviousMonthStart(now time.Time) int64 {
	current := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	return current.AddDate(0, -1, 0).Unix()
}
