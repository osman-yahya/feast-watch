package store

import "testing"

func TestSettingsDefaults(t *testing.T) {
	s := open(t)
	got, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{Interval: 10, HeartbeatMissThreshold: 3,
		Retention1mDays: 15, Retention1hDays: 75, LiveWindowMinutes: DefaultLiveWindowMinutes}
	if got != want {
		t.Fatalf("defaults: got %+v want %+v", got, want)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	s := open(t)
	in := Settings{Interval: 30, HeartbeatMissThreshold: 5,
		Retention1mDays: 7, Retention1hDays: 90, LiveWindowMinutes: 30}
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

// The live window is the newest setting, so a database written by an older
// mother has no row for it. It must read as the default rather than as 0 —
// zero would make the live view evict every sample the moment it arrives.
func TestSettingsLiveWindowDefaultsOnAnOlderDatabase(t *testing.T) {
	s := open(t)
	if err := s.SaveSettings(Settings{Interval: 10, HeartbeatMissThreshold: 3,
		Retention1mDays: 15, Retention1hDays: 75, LiveWindowMinutes: 42}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`DELETE FROM settings WHERE key = 'live_window_minutes'`); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	// The constant, not a literal: this test is about the fallback happening
	// at all, and pinning the number here would fail every time the default
	// is retuned for a reason that has nothing to do with older databases.
	if got.LiveWindowMinutes != DefaultLiveWindowMinutes {
		t.Fatalf("live window on an older database = %d, want the default %d",
			got.LiveWindowMinutes, DefaultLiveWindowMinutes)
	}
}
