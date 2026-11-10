package ptr

import (
	"bytes"
	"compress/zlib"
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/monbooru/monloader/internal/config"
)

func specTestUpdate(t *testing.T) *C2SUpdate {
	t.Helper()
	h1 := bytes.Repeat([]byte{0x11}, 32)
	h2 := bytes.Repeat([]byte{0x22}, 32)
	u := &C2SUpdate{}
	u.AddMappings(ActionPend, "spec test tag", [][]byte{h1, h2}, "")
	u.AddMappings(ActionPend, "creator:spec test artist", [][]byte{h1}, "")
	u.AddSibling(ActionPend, "speck test tag", "spec test tag", "misspelling")
	u.AddParent(ActionPend, "spec test tag", "series:spec test", "canonical")
	u.AddMappings(ActionPetition, "spec test tag", [][]byte{h2}, "not in the file")
	return u
}

func TestEncodePOSTBodyGolden(t *testing.T) {
	body, err := specTestUpdate(t).EncodePOSTBody()
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zlib.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("body is not zlib: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	h1 := strings.Repeat("11", 32)
	h2 := strings.Repeat("22", 32)
	want := `[21,2,[[[0,"client_to_server_update"],[2,[40,1,[` +
		`[2,[[[23,1,[0,["spec test tag",["` + h1 + `","` + h2 + `"]]]],""],` +
		`[[23,1,[0,["creator:spec test artist",["` + h1 + `"]]]],""],` +
		`[[23,1,[1,["speck test tag","spec test tag"]]],"misspelling"],` +
		`[[23,1,[2,["spec test tag","series:spec test"]]],"canonical"]]],` +
		`[4,[[[23,1,[0,["spec test tag",["` + h2 + `"]]]],"not in the file"]]]` +
		`]]]]]]`
	if string(raw) != want {
		t.Errorf("encoded JSON drifted:\n got %s\nwant %s", raw, want)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	body, err := specTestUpdate(t).EncodePOSTBody()
	if err != nil {
		t.Fatal(err)
	}
	upd, err := DecodeC2SUpdate(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(upd.Actions) != 2 {
		t.Fatalf("actions = %d, want 2 (pend + petition)", len(upd.Actions))
	}
	pend, petition := upd.Actions[0], upd.Actions[1]
	if pend.Action != ActionPend || petition.Action != ActionPetition {
		t.Fatalf("action order = %d,%d; want pend %d then petition %d",
			pend.Action, petition.Action, ActionPend, ActionPetition)
	}
	if len(pend.Entries) != 4 || len(petition.Entries) != 1 {
		t.Fatalf("entries = %d pend / %d petition, want 4 / 1", len(pend.Entries), len(petition.Entries))
	}
	if pend.Entries[2].ContentType != contentTypeSiblings || pend.Entries[2].Reason != "misspelling" {
		t.Errorf("sibling entry = %+v", pend.Entries[2])
	}
	if pend.Entries[3].ContentType != contentTypeParents {
		t.Errorf("parent entry = %+v", pend.Entries[3])
	}
}

func fakeGet(t *testing.T, f *fakeRepo, path, key string) ([]byte, int) {
	t.Helper()
	req, err := http.NewRequest("GET", f.URL()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Hydrus-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return body, resp.StatusCode
}

func TestParseAccountTypes(t *testing.T) {
	f := newFakeRepo(t)
	body, _ := fakeGet(t, f, "/auto_create_account_types", "")
	types, err := parseAccountTypes(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(types) != 1 {
		t.Fatalf("types = %d, want 1", len(types))
	}
	at := types[0]
	if at.Title != "Public User: Read Write" {
		t.Errorf("title = %q", at.Title)
	}
	if !strings.HasPrefix(at.Key, "074fc5d2") {
		t.Errorf("key = %q", at.Key)
	}
	if !at.CanCreateMappings() || !at.CanSend(ContribSibling) {
		t.Errorf("permissions = %v, want mappings create + petition-capable", at.Permissions)
	}
	if at.VelocityNum != 9999 {
		t.Errorf("velocity = %d", at.VelocityNum)
	}
}

func TestAccountTypeCanSendChecksEachKind(t *testing.T) {
	cases := []struct {
		name  string
		perms map[int]int
		yes   []string
		no    []string
	}{
		{"a normal account", map[int]int{0: 1, 1: 0, 2: 0},
			[]string{ContribMappingAdd, ContribMappingPetition, ContribSibling, ContribSiblingPetition, ContribParent, ContribParentPetition}, nil},
		{"petition only on mappings", map[int]int{0: 0},
			[]string{ContribMappingPetition},
			[]string{ContribMappingAdd, ContribSibling, ContribParent}},
		{"a moderator", map[int]int{0: 2, 1: 2, 2: 2},
			[]string{ContribMappingAdd, ContribSibling, ContribParentPetition}, nil},
		{"files only", map[int]int{3: 1},
			nil,
			[]string{ContribMappingAdd, ContribMappingPetition, ContribSibling, ContribSiblingPetition, ContribParent, ContribParentPetition}},
	}
	for _, c := range cases {
		at := AccountType{Permissions: c.perms}
		for _, k := range c.yes {
			if !at.CanSend(k) {
				t.Errorf("%s: CanSend(%s) = false, want true", c.name, k)
			}
		}
		for _, k := range c.no {
			if at.CanSend(k) {
				t.Errorf("%s: CanSend(%s) = true, want false", c.name, k)
			}
		}
	}
}

func TestParseAccountResponse(t *testing.T) {
	f := newFakeRepo(t)
	f.grantKey(strings.Repeat("ab", 32))
	body, _ := fakeGet(t, f, "/account", strings.Repeat("ab", 32))
	acc, err := parseAccountResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if acc.Banned || acc.Message != "" {
		t.Errorf("fresh account = banned %v message %q", acc.Banned, acc.Message)
	}
	if acc.Type.Title != "Public User: Read Write" || acc.Created == 0 || acc.Expires != 0 {
		t.Errorf("account = %+v", acc)
	}

	f.locked(func() { f.banned, f.banReason, f.message = true, "sent junk", "review the rules" })
	body2, _ := fakeGet(t, f, "/account", strings.Repeat("ab", 32))
	acc2, err := parseAccountResponse(body2)
	if err != nil {
		t.Fatal(err)
	}
	if !acc2.Banned || acc2.BanReason != "sent junk" || acc2.Message != "review the rules" {
		t.Errorf("banned account = %+v", acc2)
	}
}

func TestParseTagFilterAndBlocks(t *testing.T) {
	f := newFakeRepo(t)
	key := strings.Repeat("cd", 32)
	f.grantKey(key)
	f.mu.Lock()
	f.tagFilterRules = []any{
		[]any{"booru:", 1},
		[]any{"artist request", 1},
		[]any{"creator:allowed one", 0},
	}
	f.mu.Unlock()
	body, _ := fakeGet(t, f, "/tag_filter", key)
	filter, err := parseTagFilter(body)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		tag     string
		blocked bool
	}{
		{"booru:danbooru", true},
		{"artist request", true},
		{"creator:allowed one", false},
		{"blonde hair", false},
		{"creator:someone", false},
	}
	for _, tc := range cases {
		if got, _ := filter.Blocks(tc.tag); got != tc.blocked {
			t.Errorf("Blocks(%q) = %v, want %v", tc.tag, got, tc.blocked)
		}
	}
	var nilFilter *TagFilter
	if got, _ := nilFilter.Blocks("anything"); got {
		t.Error("nil filter must block nothing")
	}
}

func TestParseServerError(t *testing.T) {
	e := parseServerError(403, []byte(`{"error": "could not find that registration token", "exception_type": "InsufficientCredentialsException", "status_code": 403}`))
	if e.Message != "could not find that registration token" || e.ExceptionType != "InsufficientCredentialsException" {
		t.Errorf("parsed = %+v", e)
	}
	if e.Retryable() {
		t.Error("403 must not be retryable")
	}
	if !parseServerError(509, []byte(`{"error":"x"}`)).Retryable() {
		t.Error("509 must be retryable")
	}
	garbage := parseServerError(500, []byte("<html>bad gateway</html>"))
	if garbage.Message == "" {
		t.Error("unparseable body must still carry text")
	}
	if got := parseServerError(404, []byte("404 page not found")).Error(); strings.Contains(got, "()") {
		t.Errorf("Error() = %q, want no empty parentheses", got)
	}
	if got := e.Error(); !strings.Contains(got, "(InsufficientCredentialsException)") {
		t.Errorf("Error() = %q, want the type named when there is one", got)
	}
}

func TestParseHostileInputs(t *testing.T) {
	good, err := deflateJSON([]any{typeDictionary, 2, []any{}})
	if err != nil {
		t.Fatal(err)
	}
	inputs := map[string][]byte{
		"empty":          {},
		"not zlib":       []byte("plain text"),
		"truncated zlib": good[:len(good)/2],
		"wrong type": func() []byte {
			b, _ := deflateJSON([]any{99, 1, []any{}})
			return b
		}(),
		"payload not a list": func() []byte {
			b, _ := deflateJSON([]any{typeDictionary, 2, "nope"})
			return b
		}(),
		"malformed entries": func() []byte {
			b, _ := deflateJSON([]any{typeDictionary, 2, []any{[]any{"just one"}}})
			return b
		}(),
	}
	for name, in := range inputs {
		if _, err := parseResponseDict(in); err == nil {
			t.Errorf("%s: parseResponseDict returned nil error", name)
		}
		if _, err := parseAccountTypes(in); err == nil {
			t.Errorf("%s: parseAccountTypes returned nil error", name)
		}
		if _, err := parseTagFilter(in); err == nil {
			t.Errorf("%s: parseTagFilter returned nil error", name)
		}
	}
}

func TestFakeRepoSelfTest(t *testing.T) {
	f := newFakeRepo(t)

	get := func(path string) ([]byte, int) { return fakeGet(t, f, path, "") }

	body, code := get("/auto_create_registration_key?account_type_key=074fc5d2")
	if code != 200 {
		t.Fatalf("registration key answered %d", code)
	}
	regKey, err := parseKeyResponse(body, "registration_key")
	if err != nil {
		t.Fatal(err)
	}
	body, code = get("/access_key?registration_key=" + regKey)
	if code != 200 {
		t.Fatalf("access key answered %d", code)
	}
	accessKey, err := parseKeyResponse(body, "access_key")
	if err != nil {
		t.Fatal(err)
	}
	if _, code = get("/access_key?registration_key=" + regKey); code != 403 {
		t.Errorf("second redemption answered %d, want 403", code)
	}

	c := newClient(config.PTRConfig{Address: f.URL(), AccessKey: accessKey})
	if err := c.PostUpdate(context.Background(), specTestUpdate(t)); err != nil {
		t.Fatalf("PostUpdate: %v", err)
	}
	ups := f.recordedUpdates()
	if len(ups) != 1 || len(ups[0].Actions) != 2 {
		t.Fatalf("recorded = %+v", ups)
	}
	wantMapping := []any{"spec test tag", []any{strings.Repeat("11", 32), strings.Repeat("22", 32)}}
	if !reflect.DeepEqual(ups[0].Actions[0].Entries[0].Data, wantMapping) {
		t.Errorf("first pend data = %#v, want %#v", ups[0].Actions[0].Entries[0].Data, wantMapping)
	}
}
