package main

import (
	"regexp"
	"testing"
	"time"
)

func TestConvertIDIsTimestampedAndUnique(t *testing.T) {
	at := time.Date(2026, 10, 17, 14, 15, 0, 0, time.UTC)
	id := convertID(at)
	re := regexp.MustCompile(`^20261017-141500-[0-9a-f]{8}$`)
	if !re.MatchString(id) {
		t.Fatalf("convertID = %q, want 20261017-141500-<8 hex>", id)
	}
	if convertID(at) == id {
		t.Fatalf("convertID must add a random suffix so two captures in the same second do not collide")
	}
}
