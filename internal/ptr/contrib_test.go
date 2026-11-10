package ptr

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/monbooru/monloader/internal/config"
)

func contribEngine(t *testing.T, f *fakeRepo) *Engine {
	t.Helper()
	return NewEngine(config.PTRConfig{Address: f.URL(), DataPath: t.TempDir()})
}

func TestCreateContribAccount(t *testing.T) {
	f := newFakeRepo(t)
	e := contribEngine(t, f)

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	if e.HasPersonalKey() {
		t.Fatal("fresh engine claims a personal key")
	}
	key, err := e.CreateContribAccount(context.Background())
	if err != nil {
		t.Fatalf("CreateContribAccount: %v", err)
	}
	if key == "" || !e.HasPersonalKey() || e.AccessKey() != key {
		t.Errorf("key = %q, HasPersonalKey = %v", key, e.HasPersonalKey())
	}
	acc := e.CachedAccount()
	if acc == nil || acc.Type.Title != "Public User: Read Write" {
		t.Errorf("cached account = %+v", acc)
	}
	if strings.Contains(logBuf.String(), key) {
		t.Error("access key leaked into a log line")
	}

	if _, err := e.CreateContribAccount(context.Background()); !errors.Is(err, ErrAccountExists) {
		t.Errorf("second create = %v, want ErrAccountExists", err)
	}
}

func TestCreateContribAccountConcurrent(t *testing.T) {
	f := newFakeRepo(t)
	e := contribEngine(t, f)

	var wg sync.WaitGroup
	results := make([]error, 2)
	start := make(chan struct{})
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, results[i] = e.CreateContribAccount(context.Background())
		}(i)
	}
	close(start)
	wg.Wait()

	oks, exists := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			oks++
		case errors.Is(err, ErrAccountExists):
			exists++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if oks != 1 || exists != 1 {
		t.Errorf("results = %d ok, %d exists; want exactly one of each", oks, exists)
	}
}

func TestCreateContribAccountHandsBackTheKeyWhenTheAccountReadFails(t *testing.T) {
	f := newFakeRepo(t)
	f.locked(func() { f.accountDown = true })
	e := contribEngine(t, f)

	key, err := e.CreateContribAccount(context.Background())
	if err != nil || key == "" {
		t.Fatalf("CreateContribAccount = %q, %v; want the redeemed key and no error, so the caller saves it", key, err)
	}
}

func TestCreateContribAccountSkipsUnusableType(t *testing.T) {
	f := newFakeRepo(t)
	f.mu.Lock()
	f.accountTypes = []any{[]any{typeAccountType, 2, []any{
		"074fc5d250bd865d791cafe97381210d81cb59a4d3c14e262b8546225e338128",
		"Read Only",
		[]any{[]any{0, 0}, []any{2, 0}, []any{1, 0}},
		[]any{32, 1, []any{}},
		[]any{9999, 3600},
		[]any{32, 1, []any{}},
	}}}
	f.mu.Unlock()
	e := contribEngine(t, f)

	if _, err := e.CreateContribAccount(context.Background()); !errors.Is(err, errNoAutoCreation) {
		t.Errorf("create with only a read-only type = %v, want errNoAutoCreation", err)
	}
}

func TestCreateContribAccountVelocityExhausted(t *testing.T) {
	f := newFakeRepo(t)
	f.locked(func() { f.velocityExhausted = true })
	e := contribEngine(t, f)
	_, err := e.CreateContribAccount(context.Background())
	var serr *ServerError
	if !errors.As(err, &serr) || serr.Status != 400 {
		t.Fatalf("err = %v, want the repository's 400", err)
	}
	if e.HasPersonalKey() {
		t.Error("failed creation left a personal key set")
	}
}

func TestRedeemConsumedRegistrationKey(t *testing.T) {
	f := newFakeRepo(t)
	e := contribEngine(t, f)
	regKey, err := e.client.RegistrationKey(context.Background(), "074fc5d2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.client.RedeemAccessKey(context.Background(), regKey); err != nil {
		t.Fatal(err)
	}
	_, err = e.client.RedeemAccessKey(context.Background(), regKey)
	var serr *ServerError
	if !errors.As(err, &serr) || serr.Status != 403 {
		t.Fatalf("second redemption = %v, want the server's 403", err)
	}
	if !strings.Contains(serr.Message, "could not find that registration token") {
		t.Errorf("message = %q, want the server text verbatim", serr.Message)
	}
}

func TestATransportFailureKeepsTheRegistrationKeyOutOfItsError(t *testing.T) {
	f := newFakeRepo(t)
	e := contribEngine(t, f)
	f.srv.Close()
	const key = "aabbccddeeff00112233"
	_, err := e.client.RedeemAccessKey(context.Background(), key)
	if err == nil {
		t.Fatal("a closed server answered")
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("error = %q holds the registration key", err)
	}
}

func TestAFailedRefreshKeepsTheBan(t *testing.T) {
	f := newFakeRepo(t)
	f.locked(func() { f.banned, f.banReason = true, "sent junk mappings" })
	e := contribEngine(t, f)
	if _, err := e.CreateContribAccount(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.locked(func() { f.accountDown = true })
	if _, err := e.RefreshAccount(context.Background()); err == nil {
		t.Fatal("RefreshAccount succeeded against a down repository")
	}
	if acc := e.CachedAccount(); acc == nil || !acc.Banned {
		t.Errorf("cached account after a failed refresh = %+v, want the ban kept", acc)
	}
}

func TestANonJSONErrorBodyIsClipped(t *testing.T) {
	page := "<html>" + strings.Repeat("é", 400) + "</html>"
	got := parseServerError(502, []byte(page)).Message
	if len(got) > maxServerMessage+3 || !utf8.ValidString(got) {
		t.Errorf("message = %d bytes, valid %v; want at most %d valid bytes", len(got), utf8.ValidString(got), maxServerMessage+3)
	}
}

func TestAJSONErrorBodyIsClipped(t *testing.T) {
	body := `{"error": "Traceback (most recent call last):` + strings.Repeat(`\n  File \"x.py\"`, 100) + `", "exception_type": "Exception"}`
	e := parseServerError(500, []byte(body))
	if len(e.Message) > maxServerMessage+3 || !strings.HasPrefix(e.Message, "Traceback") || e.ExceptionType != "Exception" {
		t.Errorf("message = %d bytes %q, type %q; want the traceback clipped to %d", len(e.Message), e.Message[:min(len(e.Message), 40)], e.ExceptionType, maxServerMessage)
	}
}

func TestContribAccountBannedState(t *testing.T) {
	f := newFakeRepo(t)
	f.locked(func() { f.banned, f.banReason = true, "sent junk mappings" })
	e := contribEngine(t, f)
	if _, err := e.CreateContribAccount(context.Background()); err != nil {
		t.Fatalf("creation against a banning repo: %v", err)
	}
	acc := e.CachedAccount()
	if acc == nil || !acc.Banned || acc.BanReason != "sent junk mappings" {
		t.Fatalf("account = %+v, want banned with the verbatim reason", acc)
	}
	e.mu.Lock()
	cs := e.contribStatusLocked()
	e.mu.Unlock()
	if !cs.Account || !cs.Banned {
		t.Errorf("contrib status = %+v, want account+banned", cs)
	}
}

func TestSetAccessKeyClearsToPublic(t *testing.T) {
	f := newFakeRepo(t)
	e := contribEngine(t, f)
	e.SetAccessKey("aabbccdd")
	if !e.HasPersonalKey() {
		t.Fatal("personal key not registered")
	}
	e.SetAccessKey("")
	if e.HasPersonalKey() || e.AccessKey() != "" {
		t.Error("cleared key still reads personal")
	}
	if e.client.key() != config.PublicAccessKey {
		t.Error("cleared client did not fall back to the public key")
	}
}

func TestContribAccountCached(t *testing.T) {
	f := newFakeRepo(t)
	e := contribEngine(t, f)
	if _, err := e.CreateContribAccount(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.srv.Close()
	if _, err := e.ContribAccount(context.Background()); err != nil {
		t.Errorf("cached account fetch = %v, want cache hit", err)
	}
}

func TestTrackContribOutcomes(t *testing.T) {
	idx := seedDiffIndex(t)
	contrib, _ := openTestContribStore(t)

	approvedSib := commitItem(t, contrib, ContribItem{Kind: ContribSibling, Tag: "blond hair", Tag2: "blonde hair", Reason: "r"})
	approvedPar := commitItem(t, contrib, ContribItem{Kind: ContribParent, Tag: "miko", Tag2: "shrine maiden", Reason: "r"})
	pendingSib := commitItem(t, contrib, ContribItem{Kind: ContribSibling, Tag: "unrelated", Tag2: "pair", Reason: "r"})
	removedPet := commitItem(t, contrib, ContribItem{Kind: ContribMappingPetition, Tag: "gone tag", Hash: mustHash(t, h32("dd")), Reason: "r"})
	stillPet := commitItem(t, contrib, ContribItem{Kind: ContribMappingPetition, Tag: "hat", Hash: mustHash(t, h32("aa")), Reason: "r"})

	rescindedAdd := mustHash(t, h32("ee"))
	commitItem(t, contrib, ContribItem{Kind: ContribMappingAdd, Tag: "just added", Hash: rescindedAdd})
	addPet := commitItem(t, contrib, ContribItem{Kind: ContribMappingPetition, Tag: "just added", Hash: rescindedAdd, Reason: "r"})

	e := &Engine{store: idx, contrib: contrib}
	e.trackContribOutcomes()

	if row, err := contrib.LogRow(addPet); err != nil || row.Outcome != "" {
		t.Errorf("a petition of an add past coverage = %+v, %v, want pending", row, err)
	}

	e.covered = time.Now().Add(time.Hour).Unix()
	e.trackContribOutcomes()
	if row, err := contrib.LogRow(addPet); err != nil || row.Outcome != OutcomeRemoved {
		t.Errorf("a petition once coverage passed its add = %+v, %v, want removed", row, err)
	}

	want := map[int64]string{
		approvedSib: OutcomeApproved,
		approvedPar: OutcomeApproved,
		pendingSib:  "",
		removedPet:  OutcomeRemoved,
		stillPet:    "",
	}
	for id, outcome := range want {
		row, err := contrib.LogRow(id)
		if err != nil || row == nil {
			t.Fatalf("row %d: %v", id, err)
		}
		if row.Outcome != outcome {
			t.Errorf("row %d outcome = %q, want %q", id, row.Outcome, outcome)
		}
	}
}

func TestTrackContribOutcomesSettlesAnAddTheIndexLacks(t *testing.T) {
	idx := seedDiffIndex(t)
	contrib, _ := openTestContribStore(t)
	commit := func(tag, hashHex string) int64 {
		t.Helper()
		return commitItem(t, contrib, ContribItem{Kind: ContribMappingAdd, Tag: tag, Hash: mustHash(t, hashHex)})
	}
	dropped := commit("dropped tag", h32("aa"))
	held := commit("hat", h32("aa"))
	outcomeOf := func(id int64) string {
		t.Helper()
		row, err := contrib.LogRow(id)
		if err != nil || row == nil {
			t.Fatalf("row %d: %v", id, err)
		}
		return row.Outcome
	}

	e := &Engine{store: idx, contrib: contrib}
	e.trackContribOutcomes()
	if got := outcomeOf(dropped); got != OutcomeApplied {
		t.Errorf("before coverage passes the commit = %q, want applied", got)
	}

	e.covered = time.Now().Add(time.Hour).Unix()
	e.trackContribOutcomes()
	if got := outcomeOf(dropped); got != OutcomeNotTaken {
		t.Errorf("past coverage with no mapping = %q, want not_taken", got)
	}
	if got := outcomeOf(held); got != OutcomeApplied {
		t.Errorf("past coverage with the mapping = %q, want applied", got)
	}
	notTaken, err := contrib.MappingAddNotTaken("dropped tag", mustHash(t, h32("aa")))
	if err != nil || !notTaken {
		t.Errorf("MappingAddNotTaken = %v, %v, want true", notTaken, err)
	}

	if err := contrib.SetOutcome(held, OutcomeNotTaken); err != nil {
		t.Fatal(err)
	}
	e.trackContribOutcomes()
	if got := outcomeOf(held); got != OutcomeApplied {
		t.Errorf("a stamped add the index now holds = %q, want applied", got)
	}
}

func mustHash(t *testing.T, hexStr string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(hexStr)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (e *Engine) CachedAccount() *Account {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.account
}
