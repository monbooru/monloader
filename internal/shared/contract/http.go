package contract

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
)

const CodeInvalidRequest = "invalid_request"

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func WriteError(w http.ResponseWriter, status int, code, msg string) {
	WriteJSON(w, status, Error{Error: msg, Code: code})
}

// DecodeJSON answers the refusal itself when the body is not JSON or runs
// past limit bytes.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v); err != nil {
		RefuseJSONBody(w, err, limit)
		return false
	}
	return true
}

func RefuseJSONBody(w http.ResponseWriter, err error, limit int64) {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		WriteError(w, http.StatusRequestEntityTooLarge, CodeFileTooLarge, fmt.Sprintf("JSON body exceeds %d MiB", limit>>20))
		return
	}
	WriteError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid JSON body")
}

func PathInt64(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	v, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil {
		WriteError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid "+name)
		return 0, false
	}
	return v, true
}

// PageParams reads a 1-based page and a limit capped at max, each falling
// back to its default when absent or not positive. The page stops where
// (page-1)*limit would wrap negative and serve the first page.
func PageParams(page, limit string, def, max int) (int, int) {
	p, l := 1, def
	if n, err := strconv.Atoi(page); err == nil && n > 0 {
		p = n
	}
	if n, err := strconv.Atoi(limit); err == nil && n > 0 {
		l = min(n, max)
	}
	return min(p, math.MaxInt/l), l
}
