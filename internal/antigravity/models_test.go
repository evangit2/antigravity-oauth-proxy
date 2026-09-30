package antigravity

import (
	"encoding/json"
	"testing"
)

func TestParsedQuota(t *testing.T) {
	m := AvailableModel{QuotaInfo: json.RawMessage(`{"remainingFraction": 0.99718, "resetTime": "2026-10-01T01:23:56Z"}`)}
	q, ok := m.ParsedQuota()
	if !ok || q.RemainingFraction != 0.99718 || q.ResetTime != "2026-10-01T01:23:56Z" {
		t.Fatalf("got %+v %v", q, ok)
	}
	if _, ok := (AvailableModel{}).ParsedQuota(); ok {
		t.Fatal("empty quota should not parse")
	}
	if _, ok := (AvailableModel{QuotaInfo: json.RawMessage(`{bad}`)}).ParsedQuota(); ok {
		t.Fatal("malformed quota should not parse")
	}
}
