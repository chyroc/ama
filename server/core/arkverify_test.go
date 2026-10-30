// Tests for the three-way classification of LiveArkVerifier.
//
// httptest starts a fake Ark that returns different status codes with response bodies shaped like
// real captured ones ({"error":{...}}), and asserts that Verify classifies only by structured
// status code / error code:
//
//	401/403                        → InvalidKey
//	404 + code=ManagedAgentNotOpen → NotActivated (and the account id can be extracted)
//	200                            → OK
//	500 / 429 / 404 with empty code → Skipped (dependency failure / not recognized; let it pass, lenient)
//
// The last group is **the target of mutation testing**: if someone weakens the not-activated check
// to "just look at 404", a "resource does not exist" 404 would be misjudged as not activated, and
// this case turns red at once.

package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chyroc/ama/server/model/apimodel"
	arkcredsvc "github.com/chyroc/ama/server/service/arkcred"
)

// fakeArk answers /api/v3/agents with the preset status code and error body.
//
// request_id goes in the X-Request-Id response header (the SDK only reads the header, not the
// field in the body; real packet captures behave the same way).
func fakeArk(t *testing.T, status int, requestID, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/agents" {
			t.Errorf("探针应只读 GET /api/v3/agents，实际 %s %s", r.Method, r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("探针必须是无副作用的 GET，实际 %s", r.Method)
		}
		if requestID != "" {
			w.Header().Set("X-Request-Id", requestID)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runVerify(t *testing.T, srv *httptest.Server) arkcredsvc.VerifyResult {
	t.Helper()
	v := &LiveArkVerifier{BaseURLOverride: srv.URL + "/api/v3"}
	return v.Verify(context.Background(), "ark-test-key-1234567890abcd", apimodel.RegionCNBeijing)
}

func TestVerifyRejectsInvalidKey(t *testing.T) {
	// Real response text (captured from a stale invalid key in production):
	// {"error":{"code":"AuthenticationError","message":"The API key doesn't exist.",...}}
	srv := fakeArk(t, http.StatusUnauthorized, "req-401",
		`{"error":{"code":"AuthenticationError","message":"The API key doesn't exist.","type":"Unauthorized"}}`)
	got := runVerify(t, srv)
	if got.Status != arkcredsvc.VerifyStatusInvalidKey {
		t.Fatalf("401 应判 InvalidKey，实际 %s", got.Status)
	}
	if got.RequestID != "req-401" {
		t.Errorf("request_id 应透传，实际 %q", got.RequestID)
	}
}

func TestVerifyRejectsInvalidKey403(t *testing.T) {
	// 403 is in the same bucket as 401 (IsAuthError accepts both).
	srv := fakeArk(t, http.StatusForbidden, "",
		`{"error":{"code":"Forbidden","message":"forbidden","type":"Forbidden"}}`)
	if got := runVerify(t, srv); got.Status != arkcredsvc.VerifyStatusInvalidKey {
		t.Fatalf("403 应判 InvalidKey，实际 %s", got.Status)
	}
}

func TestVerifyNotActivated(t *testing.T) {
	// Real response text (captured during production failures):
	// 404 + code=ManagedAgentNotOpen, with the account id in the message.
	srv := fakeArk(t, http.StatusNotFound, "req-na",
		`{"error":{"code":"ManagedAgentNotOpen","message":"Your account 2101581202 has not activated the managed agent.","type":"Not Found"}}`)
	got := runVerify(t, srv)
	if got.Status != arkcredsvc.VerifyStatusNotActivated {
		t.Fatalf("应判 NotActivated，实际 %s（cause=%v）", got.Status, got.Cause)
	}
	if got.AccountID != "2101581202" {
		t.Errorf("应从原文提出账号 id，实际 %q", got.AccountID)
	}
	if got.RequestID != "req-na" {
		t.Errorf("request_id 应透传，实际 %q", got.RequestID)
	}
}

func TestVerifyOK(t *testing.T) {
	srv := fakeArk(t, http.StatusOK, "",
		`{"data":[]}`)
	if got := runVerify(t, srv); got.Status != arkcredsvc.VerifyStatusOK {
		t.Fatalf("200 应判 OK，实际 %s（cause=%v）", got.Status, got.Cause)
	}
}

func TestVerifySkippedOn5xx(t *testing.T) {
	srv := fakeArk(t, http.StatusInternalServerError, "",
		`{"error":{"code":"InternalServiceError","message":"boom"}}`)
	got := runVerify(t, srv)
	if got.Status != arkcredsvc.VerifyStatusSkipped {
		t.Fatalf("5xx 必须判 Skipped（放行保存），实际 %s", got.Status)
	}
	if got.Cause == nil {
		t.Error("Skipped 要带技术原因供日志排查")
	}
}

func TestVerifySkippedOn429(t *testing.T) {
	// Rate limiting means Ark is temporarily busy, not a problem with the key; it must pass.
	srv := fakeArk(t, http.StatusTooManyRequests, "",
		`{"error":{"code":"RateLimitExceeded","message":"slow down"}}`)
	if got := runVerify(t, srv); got.Status != arkcredsvc.VerifyStatusSkipped {
		t.Fatalf("429 必须判 Skipped，实际 %s", got.Status)
	}
}

// TestVerifyPlain404IsSkipped is a mutation target: **not-activated must not be judged by 404 alone**.
//
// The same 404 with an empty code means "resource does not exist" (ma.IsNotFound), and is handled
// the exact opposite way from "account not activated" (code=ManagedAgentNotOpen). Looking only at
// the status code would misjudge it as not activated, and on save would push the user to activate
// an account that is in fact already activated.
func TestVerifyPlain404IsSkipped(t *testing.T) {
	srv := fakeArk(t, http.StatusNotFound, "",
		`{"error":{"code":"","message":"not found","type":"Not Found"}}`)
	got := runVerify(t, srv)
	if got.Status != arkcredsvc.VerifyStatusSkipped {
		t.Fatalf("空 code 的 404 必须判 Skipped，实际 %s", got.Status)
	}
}

// TestVerifyNetworkFailureIsSkipped: cannot connect (dependency down) must also pass; it is not a bad key.
func TestVerifyNetworkFailureIsSkipped(t *testing.T) {
	srv := fakeArk(t, http.StatusBadGateway, "", `{"error":{"code":"x"}}`)
	srv.Close() // close it right away to simulate a connection failure
	got := runVerify(t, srv)
	if got.Status != arkcredsvc.VerifyStatusSkipped || got.Cause == nil {
		t.Fatalf("连接失败应判 Skipped 并带原因，实际 %+v", got)
	}
}
