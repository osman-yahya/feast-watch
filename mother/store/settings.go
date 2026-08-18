package store

import (
	"fmt"
	"strconv"
)

// Settings are the panel-configurable knobs (spec: "Configurable from the panel").
//
// Agent version rollout deliberately lives on the server row rather than here:
// see Store.SetDesiredVersion.
// retention_raw_hours is deliberately absent: there is no raw tier left for it
// to bound (see schema.go). The key is dropped from stored settings by
// migration and ignored on the way in, so an older caller still sending it is
// not rejected — it simply no longer describes anything.
type Settings struct {
	Interval               int `json:"interval"`
	HeartbeatMissThreshold int `json:"heartbeat_miss_threshold"`
	Retention1mDays        int `json:"retention_1m_days"`
	Retention1hDays        int `json:"retention_1h_days"`
}

var defaultSettings = Settings{
	Interval: 10, HeartbeatMissThreshold: 3,
	Retention1mDays: 15, Retention1hDays: 75,
}

func (s *Store) GetSettings() (Settings, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return Settings{}, err
	}
	defer rows.Close()

	out := defaultSettings
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return Settings{}, err
		}
		// A non-numeric value must not read as 0: every retention field feeds
		// a `now - value` cutoff, so silently reading 0 would turn the next
		// sweep into "delete this whole tier".
		n, err := strconv.Atoi(v)
		if err != nil {
			return Settings{}, fmt.Errorf("setting %q holds a non-numeric value %q: %w", k, v, err)
		}
		switch k {
		case "interval":
			out.Interval = n
		case "heartbeat_miss_threshold":
			out.HeartbeatMissThreshold = n
		case "retention_1m_days":
			out.Retention1mDays = n
		case "retention_1h_days":
			out.Retention1hDays = n
		}
	}
	return out, rows.Err()
}

func (s *Store) SaveSettings(in Settings) error {
	pairs := map[string]string{
		"interval":                 strconv.Itoa(in.Interval),
		"heartbeat_miss_threshold": strconv.Itoa(in.HeartbeatMissThreshold),
		"retention_1m_days":        strconv.Itoa(in.Retention1mDays),
		"retention_1h_days":        strconv.Itoa(in.Retention1hDays),
	}
	for k, v := range pairs {
		if _, err := s.db.Exec(
			`INSERT INTO settings (key, value) VALUES (?,?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return err
		}
	}
	return nil
}
