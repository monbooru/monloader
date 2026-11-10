package ptr

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

type fakeRepo struct {
	srv *httptest.Server

	mu                sync.Mutex
	accountTypes      []any
	velocityExhausted bool
	registrationKeys  map[string]bool
	accessKeys        map[string]bool
	banned            bool
	banReason         string
	message           string
	tagFilterRules    []any
	fail509           int
	accountDown       bool
	updates           []DecodedUpdate
}

func ptrLikeAccountType() []any {
	return []any{typeAccountType, 2, []any{
		"074fc5d250bd865d791cafe97381210d81cb59a4d3c14e262b8546225e338128",
		"Public User: Read Write",
		[]any{[]any{0, 1}, []any{2, 0}, []any{1, 0}},
		[]any{32, 1, []any{}},
		[]any{9999, 3600},
		[]any{32, 1, []any{}},
	}}
}

func newFakeRepo(t *testing.T) *fakeRepo {
	t.Helper()
	f := &fakeRepo{
		accountTypes:     []any{ptrLikeAccountType()},
		registrationKeys: map[string]bool{},
		accessKeys:       map[string]bool{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auto_create_account_types", f.handleAccountTypes)
	mux.HandleFunc("GET /auto_create_registration_key", f.handleRegistrationKey)
	mux.HandleFunc("GET /access_key", f.handleAccessKey)
	mux.HandleFunc("GET /account", f.handleAccount)
	mux.HandleFunc("GET /tag_filter", f.handleTagFilter)
	mux.HandleFunc("POST /update", f.handleUpdate)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRepo) URL() string { return f.srv.URL }

func (f *fakeRepo) locked(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *fakeRepo) grantKey(key string) {
	f.mu.Lock()
	f.accessKeys[key] = true
	f.mu.Unlock()
}

func (f *fakeRepo) recordedUpdates() []DecodedUpdate {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]DecodedUpdate, len(f.updates))
	copy(out, f.updates)
	return out
}

func (f *fakeRepo) respondDict(w http.ResponseWriter, entries ...[2]any) {
	pairs := make([]any, 0, len(entries))
	for _, e := range entries {
		pairs = append(pairs, []any{[]any{metaPlain, e[0]}, e[1]})
	}
	body, err := deflateJSON([]any{typeDictionary, 2, pairs})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func respondError(w http.ResponseWriter, status int, exceptionType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": msg, "exception_type": exceptionType, "status_code": status,
	})
}

func plain(v any) []any        { return []any{metaPlain, v} }
func serialisable(v any) []any { return []any{metaSerialisable, v} }

func (f *fakeRepo) handleAccountTypes(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	metas := make([]any, 0, len(f.accountTypes))
	for _, at := range f.accountTypes {
		metas = append(metas, serialisable(at))
	}
	f.mu.Unlock()
	list := []any{typeList, 3, metas}
	f.respondDict(w, [2]any{"account_types", serialisable(list)})
}

func (f *fakeRepo) handleRegistrationKey(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.velocityExhausted {
		respondError(w, 400, "BadRequestException",
			"there are no new authentication accounts available for now")
		return
	}
	key := randomHex()
	f.registrationKeys[key] = true
	f.respondDict(w, [2]any{"registration_key", plain(key)})
}

func (f *fakeRepo) handleAccessKey(w http.ResponseWriter, r *http.Request) {
	regKey := r.URL.Query().Get("registration_key")
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.registrationKeys[regKey] {
		respondError(w, 403, "InsufficientCredentialsException",
			"could not find that registration token")
		return
	}
	delete(f.registrationKeys, regKey)
	key := randomHex()
	f.accessKeys[key] = true
	f.respondDict(w, [2]any{"access_key", plain(key)})
}

func (f *fakeRepo) authed(r *http.Request) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accessKeys[r.Header.Get("Hydrus-Key")]
}

func (f *fakeRepo) handleAccount(w http.ResponseWriter, r *http.Request) {
	if !f.authed(r) {
		respondError(w, 403, "InsufficientCredentialsException", "did not recognise that access key")
		return
	}
	f.mu.Lock()
	if f.accountDown {
		f.mu.Unlock()
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	bannedInfo := plain(nil)
	if f.banned {
		bannedInfo = serialisable([]any{typeList, 3, []any{plain(f.banReason), plain(1700000000), plain(nil)}})
	}
	innerEntries := []any{
		[]any{plain("banned_info"), bannedInfo},
		[]any{plain("message"), plain(f.message)},
	}
	f.mu.Unlock()
	innerJSON, err := json.Marshal([]any{typeDictionary, 2, innerEntries})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	accountTuple := []any{
		randomHex(),
		ptrLikeAccountType(),
		1720000000,
		nil,
		string(innerJSON),
	}
	f.respondDict(w, [2]any{"account", plain(accountTuple)})
}

func (f *fakeRepo) handleTagFilter(w http.ResponseWriter, r *http.Request) {
	if !f.authed(r) {
		respondError(w, 403, "InsufficientCredentialsException", "did not recognise that access key")
		return
	}
	f.mu.Lock()
	rules := f.tagFilterRules
	f.mu.Unlock()
	if rules == nil {
		rules = []any{}
	}
	f.respondDict(w, [2]any{"tag_filter", serialisable([]any{typeTagFilter, 1, rules})})
}

func (f *fakeRepo) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if !f.authed(r) {
		respondError(w, 403, "InsufficientCredentialsException", "did not recognise that access key")
		return
	}
	f.mu.Lock()
	if f.banned {
		reason := f.banReason
		f.mu.Unlock()
		respondError(w, 403, "InsufficientCredentialsException", "you are banned: "+reason)
		return
	}
	if f.fail509 > 0 {
		f.fail509--
		f.mu.Unlock()
		respondError(w, 509, "BandwidthException", "no bandwidth for you right now")
		return
	}
	f.mu.Unlock()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		respondError(w, 400, "BadRequestException", err.Error())
		return
	}
	upd, err := DecodeC2SUpdate(body)
	if err != nil {
		respondError(w, 400, "BadRequestException", err.Error())
		return
	}
	f.mu.Lock()
	f.updates = append(f.updates, upd)
	f.mu.Unlock()
	w.WriteHeader(200)
}

func randomHex() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
