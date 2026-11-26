package cron

import (
	"testing"
	"time"
)

func TestParseAndDueEvery(t *testing.T) {
	spec, err := Parse("@every 5s")
	if err != nil {
		t.Fatalf("Parse(@every 5s) error: %v", err)
	}
	base := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	if !spec.Due(base, time.Time{}) {
		t.Fatal("expected due when lastRun is zero")
	}
	if spec.Due(base.Add(4*time.Second), base) {
		t.Fatal("expected not due before 5s elapsed")
	}
	if !spec.Due(base.Add(5*time.Second), base) {
		t.Fatal("expected due at 5s elapsed")
	}
}

func TestParseAndDueStandardCron(t *testing.T) {
	spec, err := Parse("*/15 9-17 * * 1-5")
	if err != nil {
		t.Fatalf("Parse standard cron error: %v", err)
	}
	wed := time.Date(2026, 11, 25, 10, 30, 0, 0, time.UTC)
	if !spec.Due(wed, time.Time{}) {
		t.Fatal("expected wed 10:30 to be due")
	}
	if spec.Due(wed.Add(20*time.Second), wed) {
		t.Fatal("expected same minute not to re-fire")
	}
	if !spec.Due(wed.Add(15*time.Minute), wed) {
		t.Fatal("expected wed 10:45 to be due")
	}
	sun := time.Date(2026, 11, 29, 10, 30, 0, 0, time.UTC)
	if spec.Due(sun, time.Time{}) {
		t.Fatal("expected sunday not to match 1-5")
	}
}

func TestParseInvalid(t *testing.T) {
	for _, expr := range []string{"", "* * *", "@every -1s", "60 * * * *"} {
		if _, err := Parse(expr); err == nil {
			t.Fatalf("expected error for %q", expr)
		}
	}
}
