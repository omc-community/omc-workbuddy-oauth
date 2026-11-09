package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// TestResetHintReadsTheStatedInstant covers the two shapes WorkBuddy writes and
// what it must refuse to read: no offset, an instant already past, a wall clock
// no zone can hold, and a date so far out that it is a misread.
func TestResetHintReadsTheStatedInstant(t *testing.T) {
	now := time.Date(2026, 11, 9, 20, 0, 0, 0, beijingZone)
	tests := []struct {
		name    string
		message string
		want    int
	}{
		{
			name:    "english reset line",
			message: "usage exceeds frequency limit, your usage will reset at 2026-11-09 21:01:11 UTC+8",
			want:    3671,
		},
		{
			name:    "chinese reset line",
			message: "WorkBuddy is limiting this account: 2026-11-09 21:01:11 UTC+8 重置，您也可以切换其他模型继续使用。",
			want:    3671,
		},
		{
			name:    "half-hour offset",
			message: "will reset at 2026-11-09 18:31:11 UTC+5:30",
			want:    3671,
		},
		{
			name:    "negative offset",
			message: "will reset at 2026-11-09 08:01:11 UTC-5",
			want:    3671,
		},
		{
			name:    "no offset is no deadline",
			message: "your usage will reset at 2026-11-09 21:01:11",
			want:    0,
		},
		{
			name:    "already past",
			message: "your usage will reset at 2026-11-09 19:00:00 UTC+8",
			want:    0,
		},
		{
			name:    "impossible wall clock",
			message: "your usage will reset at 2026-11-09 21:61:11 UTC+8",
			want:    0,
		},
		{
			name:    "past the month bound",
			message: "your usage will reset at 2027-01-09 21:01:11 UTC+8",
			want:    0,
		},
		{
			name:    "no instant at all",
			message: "usage exceeds frequency limit",
			want:    0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resetHint(tc.message, now); got != tc.want {
				t.Fatalf("resetHint(%q) = %d, want %d", tc.message, got, tc.want)
			}
		})
	}
}

// TestClassifyCarriesTheResetAsRetryAfter pins the contract with CPA: a 429 whose
// message names when the allowance returns must say so in the error, because that
// number is the cooldown CPA applies to the model. Without it CPA cools from its
// own one-second backoff and the account is retried long before it is ready.
func TestClassifyCarriesTheResetAsRetryAfter(t *testing.T) {
	reset := time.Now().Add(90 * time.Minute).In(beijingZone).Format("2006-01-02 15:04:05")
	body, err := json.Marshal(map[string]any{
		"code": 14018,
		"msg":  "usage exceeds frequency limit, your usage will reset at " + reset + " UTC+8",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = classifyUpstream(testAccount(t), http.StatusTooManyRequests, nil, body)
	var perr *pluginabi.Error
	if !errors.As(err, &perr) || perr.Code != "rate_limited" || perr.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("err = %#v", err)
	}
	if perr.RetryAfterSeconds < 60*89 || perr.RetryAfterSeconds > 60*91 {
		t.Fatalf("RetryAfterSeconds = %d, want about 90 minutes", perr.RetryAfterSeconds)
	}
}

// TestRetryHintPrefersTheHeader pins which statement wins: a Retry-After WorkBuddy
// sends is machine-stated, so it outranks the instant the message carries.
func TestRetryHintPrefersTheHeader(t *testing.T) {
	now := time.Date(2026, 11, 9, 20, 0, 0, 0, beijingZone)
	message := "your usage will reset at 2026-11-09 21:01:11 UTC+8"
	if got := retryHint(http.Header{"Retry-After": {"45"}}, message, now); got != 45 {
		t.Fatalf("delta seconds = %d, want 45", got)
	}
	at := now.Add(30 * time.Minute).UTC().Format(http.TimeFormat)
	if got := retryHint(http.Header{"Retry-After": {at}}, message, now); got != 1800 {
		t.Fatalf("http date = %d, want 1800", got)
	}
	if got := retryHint(nil, message, now); got != 3671 {
		t.Fatalf("message instant = %d, want 3671", got)
	}
	if got := retryHint(http.Header{"Retry-After": {"soon"}}, message, now); got != 3671 {
		t.Fatalf("unreadable header = %d, want the message instant", got)
	}
}

// TestClassifyLeavesAnUnstatedResetAlone is the other half of the contract: a 429
// that names no instant still reaches CPA as one, and invents no deadline.
func TestClassifyLeavesAnUnstatedResetAlone(t *testing.T) {
	body, err := json.Marshal(map[string]any{"code": 14018, "msg": "usage exceeds frequency limit"})
	if err != nil {
		t.Fatal(err)
	}
	err = classifyUpstream(testAccount(t), http.StatusTooManyRequests, nil, body)
	var perr *pluginabi.Error
	if !errors.As(err, &perr) || perr.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("err = %#v", err)
	}
	if perr.RetryAfterSeconds != 0 {
		t.Fatalf("RetryAfterSeconds = %d, want none", perr.RetryAfterSeconds)
	}
}

// TestFrequencyLimitStaysModelScoped pins the scope of WorkBuddy's 14018: the
// refusal names an instant for this model and tells the user another model still
// works, so the whole credential must stay in rotation.
func TestFrequencyLimitStaysModelScoped(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"code": 14018,
		"msg":  "usage exceeds frequency limit, your usage will reset at 2026-11-09 21:01:11 UTC+8",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = classifyUpstream(testAccount(t), http.StatusTooManyRequests, nil, body)
	var perr *pluginabi.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err = %#v", err)
	}
	if perr.CredentialScoped {
		t.Fatalf("CredentialScoped = true, want this model only")
	}
}

// testAccount is the credential a classification test reports against.
func testAccount(t *testing.T) credential {
	t.Helper()
	c, err := readCredential(testCredential(siteWorkBuddy))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
