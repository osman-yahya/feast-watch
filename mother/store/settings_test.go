package store

import "testing"

func TestSettingsDefaults(t *testing.T) {
	s := open(t)
	got, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{Interval: 10, HeartbeatMissThreshold: 3,
		Retention1mDays: 15, Retention1hDays: 75}
	if got != want {
		t.Fatalf("defaults: got %+v want %+v", got, want)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	s := open(t)
	in := Settings{Interval: 30, HeartbeatMissThreshold: 5,
		Retention1mDays: 7, Retention1hDays: 90}
	if err := s.SaveSettings(in); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSettings()
	if got != in {
		t.Fatalf("got %+v want %+v", got, in)
	}
}

// A stored value that is not a number must not read as zero: every retention
// field feeds a `now - value` cutoff, so a corrupt row would silently turn the
// next sweep into "delete the whole tier".
func TestSettingsRejectCorruptStoredValue(t *testing.T) {
	s := open(t)
	if _, err := s.DB().Exec(
		`INSERT INTO settings (key, value) VALUES ('retention_1m_days', 'not-a-number')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSettings(); err == nil {
		t.Fatal("a non-numeric stored setting must surface as an error, not as 0")
	}
}
