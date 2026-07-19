package store

import "testing"

func TestSettingsDefaults(t *testing.T) {
	s := open(t)
	got, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	want := Settings{Interval: 10, HeartbeatMissThreshold: 3, RetentionRawHours: 48,
		Retention1mDays: 15, Retention1hDays: 75, DesiredVersion: ""}
	if got != want {
		t.Fatalf("defaults: got %+v want %+v", got, want)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	s := open(t)
	in := Settings{Interval: 30, HeartbeatMissThreshold: 5, RetentionRawHours: 24,
		Retention1mDays: 7, Retention1hDays: 90, DesiredVersion: "v1.3.0"}
	if err := s.SaveSettings(in); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSettings()
	if got != in {
		t.Fatalf("got %+v want %+v", got, in)
	}
}
