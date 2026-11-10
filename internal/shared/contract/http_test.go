package contract

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func refusal(t *testing.T, rec *httptest.ResponseRecorder) Error {
	t.Helper()
	var e Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("body %q: %v", rec.Body, err)
	}
	return e
}

func TestDecodeJSONRefusesAnOversizeBodyAsFileTooLarge(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"a":"`+strings.Repeat("x", 2<<20)+`"}`))
	var v map[string]string
	if DecodeJSON(rec, r, &v, 1<<20) {
		t.Fatal("an oversize body decoded")
	}
	if e := refusal(t, rec); rec.Code != http.StatusRequestEntityTooLarge || e.Code != CodeFileTooLarge || e.Error != "JSON body exceeds 1 MiB" {
		t.Errorf("got %d %+v", rec.Code, e)
	}
}

func TestDecodeJSONRefusesBadJSONAsInvalidRequest(t *testing.T) {
	rec := httptest.NewRecorder()
	var v map[string]string
	if DecodeJSON(rec, httptest.NewRequest("POST", "/", strings.NewReader("{")), &v, 1<<20) {
		t.Fatal("a broken body decoded")
	}
	if e := refusal(t, rec); rec.Code != http.StatusBadRequest || e.Code != CodeInvalidRequest {
		t.Errorf("got %d %+v", rec.Code, e)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestPathInt64RefusesANonNumber(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/x", nil)
	r.SetPathValue("id", "x1")
	if _, ok := PathInt64(rec, r, "id"); ok {
		t.Fatal("x1 parsed")
	}
	if e := refusal(t, rec); rec.Code != http.StatusBadRequest || e.Error != "invalid id" {
		t.Errorf("got %d %+v", rec.Code, e)
	}
}

func TestPageParams(t *testing.T) {
	for _, tc := range []struct {
		page, limit  string
		wantP, wantL int
	}{
		{"", "", 1, 40},
		{"3", "10", 3, 10},
		{"0", "-5", 1, 40},
		{"x", "500", 1, 200},
		{"4611686018427387904", "10", math.MaxInt / 10, 10},
	} {
		p, l := PageParams(tc.page, tc.limit, 40, 200)
		if p != tc.wantP || l != tc.wantL {
			t.Errorf("PageParams(%q, %q) = %d, %d, want %d, %d", tc.page, tc.limit, p, l, tc.wantP, tc.wantL)
		}
		if (p-1)*l < 0 {
			t.Errorf("PageParams(%q, %q): offset wraps", tc.page, tc.limit)
		}
	}
}
