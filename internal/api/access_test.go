package api

import (
	"testing"
	"time"
)

func TestTOTPMatchesRFC6238(t *testing.T) {
	// RFC 6238 appendix B, SHA-1, secret "12345678901234567890" (8-digit vectors; ours are the last 6 digits)
	secret := []byte("12345678901234567890")
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037"} {
		if got := totpAt(secret, unix/30); got != want {
			t.Errorf("t=%d: got %s want %s", unix, got, want)
		}
	}
	now := time.Unix(1234567890, 0)
	if !totpValid(secret, "005924", now) || !totpValid(secret, " 005 924 ", now) {
		t.Error("the current code must be accepted")
	}
	if !totpValid(secret, totpAt(secret, 1234567890/30-1), now) {
		t.Error("the previous code must be accepted (clock drift)")
	}
	if totpValid(secret, totpAt(secret, 1234567890/30-2), now) || totpValid(secret, "000000", now) || totpValid(secret, "", now) {
		t.Error("an old or wrong code was accepted")
	}
}

func TestRouteLevels(t *testing.T) {
	for pattern, want := range map[string]int{
		"GET /api/backups": 1, "GET /api/backups/{id}": 1, "GET /api/backups/{id}/download": 2, "GET /api/backups/{id}/rows": 2,
		"POST /api/plans/{id}/run": 2, "POST /api/plans": 3, "DELETE /api/backups/{id}": 3, "POST /api/backups/{id}/restore": 3,
		"POST /api/backups/{id}/restore-table": 3, "PUT /api/settings": 3, "GET /api/settings": 3, "GET /api/audit": 3, "POST /api/users": 3,
		"GET /api/users": 3, "POST /api/tokens": 3, "POST /api/targets": 3, "GET /metrics": 1, "POST /api/logout": 1, "POST /api/me/password": 1,
	} {
		if got := levelFor(pattern); got != want {
			t.Errorf("%s: level %d, want %d", pattern, got, want)
		}
	}
}
