package pairing

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

type offer struct {
	Request
	URL string
}

func TestAnEmbeddingOfferKeepsItsOwnFields(t *testing.T) {
	s := NewStore[offer]()
	id, ok := s.Create(offer{Request: Request{App: "monedit", Scopes: []string{"read"}}, URL: "http://a"})
	if !ok {
		t.Fatal("create failed")
	}
	got, ok := s.Get(id)
	if !ok || got.URL != "http://a" || got.App != "monedit" || got.State != Pending || got.ID != id {
		t.Errorf("got %+v", got)
	}
}

func TestClaimWinsOnceAndUnclaimAllowsRetry(t *testing.T) {
	s := NewStore[Request]()
	id, _ := s.Create(Request{App: "monloader"})
	if _, won := s.Claim(id); won {
		t.Fatal("a pending request was claimed")
	}
	if !s.SetState(id, Approved) {
		t.Fatal("approve failed")
	}
	if s.SetState(id, Denied) {
		t.Error("a decided request changed state")
	}
	if _, won := s.Claim(id); !won {
		t.Fatal("first claim should win")
	}
	if _, won := s.Claim(id); won {
		t.Fatal("second claim should not win while claimed")
	}
	s.Unclaim(id)
	if _, won := s.Claim(id); !won {
		t.Fatal("claim after unclaim should win so a failed mint can retry")
	}
}

func TestARepeatRequestReplacesThePendingOne(t *testing.T) {
	s := NewStore[Request]()
	first, _ := s.Create(Request{App: "monsender"})
	second, _ := s.Create(Request{App: "monsender"})
	if first == second {
		t.Fatal("the repeat kept the first id")
	}
	if _, ok := s.Get(first); ok {
		t.Error("the earlier pending request should have been replaced")
	}
	pending := s.ListPending()
	if len(pending) != 1 || pending[0].ID != second {
		t.Errorf("pending = %+v, want only the newest request", pending)
	}
}

func TestTwoLabelsOfOneAppWaitSideBySide(t *testing.T) {
	s := NewStore[Request]()
	gpu, _ := s.Create(Request{App: "monworker", Label: "gpu-box"})
	desk, _ := s.Create(Request{App: "monworker", Label: "desktop"})
	again, _ := s.Create(Request{App: "monworker", Label: "gpu-box"})
	if _, ok := s.Get(gpu); ok {
		t.Error("a repeat under the same label kept the earlier request")
	}
	for _, id := range []string{desk, again} {
		if _, ok := s.Get(id); !ok {
			t.Errorf("request %s was dropped", id)
		}
	}
	s.DropPending("monworker")
	if p := s.ListPending(); len(p) != 0 {
		t.Errorf("DropPending left %+v", p)
	}
}

func TestADecidedRequestSurvivesARepeat(t *testing.T) {
	s := NewStore[Request]()
	first, _ := s.Create(Request{App: "monsender"})
	s.SetState(first, Approved)
	s.Create(Request{App: "monsender"})
	if r, ok := s.Get(first); !ok || r.State != Approved {
		t.Errorf("an approval awaiting its claim was dropped: %+v", r)
	}
}

func TestListApprovedHoldsAnApprovalUntilItsClaim(t *testing.T) {
	s := NewStore[Request]()
	approved, _ := s.Create(Request{App: "monloader"})
	s.Create(Request{App: "monsender"})
	denied, _ := s.Create(Request{App: "monedit"})
	s.SetState(approved, Approved)
	s.SetState(denied, Denied)
	if got := s.ListApproved(); len(got) != 1 || got[0].ID != approved {
		t.Fatalf("approved = %+v, want only the approved request", got)
	}
	s.Claim(approved)
	if got := s.ListApproved(); len(got) != 0 {
		t.Errorf("a claimed approval is still listed: %+v", got)
	}
	s.Unclaim(approved)
	if got := s.ListApproved(); len(got) != 1 {
		t.Errorf("an unclaimed approval is not listed again: %+v", got)
	}
}

func TestPendingIsCappedAcrossApps(t *testing.T) {
	s := NewStore[Request]()
	for i := range MaxPending {
		if _, ok := s.Create(Request{App: fmt.Sprintf("app-%d", i)}); !ok {
			t.Fatalf("request %d refused", i)
		}
	}
	if _, ok := s.Create(Request{App: "one-more"}); ok {
		t.Error("a request past the cap was accepted")
	}
	if _, ok := s.Create(Request{App: "app-0"}); !ok {
		t.Error("a repeat from a waiting app was refused at the cap")
	}
}

func TestAnAgedRequestIsSwept(t *testing.T) {
	s := NewStore[Request]()
	id, _ := s.Create(Request{App: "monsender"})
	s.m[id].CreatedAt = time.Now().Add(-pendingTTL - time.Second)
	if len(s.ListPending()) != 0 {
		t.Error("an aged request is still listed")
	}
	if _, ok := s.Get(id); ok {
		t.Error("an aged request is still held")
	}
}

func TestARequestExpiresWhenTheSweepDropsIt(t *testing.T) {
	s := NewStore[Request]()
	id, _ := s.Create(Request{App: "monsender"})
	req, _ := s.Get(id)
	if got := req.ExpiresAt().Sub(req.CreatedAt); got != pendingTTL {
		t.Errorf("ExpiresAt is %s after creation, want %s", got, pendingTTL)
	}
}

func TestDropPendingAndRemove(t *testing.T) {
	s := NewStore[Request]()
	a, _ := s.Create(Request{App: "a"})
	b, _ := s.Create(Request{App: "b"})
	s.DropPending("a")
	if _, ok := s.Get(a); ok {
		t.Error("DropPending kept the request")
	}
	s.Remove(b)
	if _, ok := s.Get(b); ok {
		t.Error("Remove kept the request")
	}
}

func TestPendingKeepsTheOrderTheyArrived(t *testing.T) {
	s := NewStore[Request]()
	var want []string
	for i := range 8 {
		label := fmt.Sprintf("u%d", i)
		if _, ok := s.Create(Request{App: "monworker", Label: label}); !ok {
			t.Fatal("create")
		}
		want = append(want, label)
	}
	for range 20 {
		var got []string
		for _, r := range s.ListPending() {
			got = append(got, r.Label)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}
