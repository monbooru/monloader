package contrib

import (
	"context"
	"errors"
	"testing"

	"github.com/monbooru/monloader/internal/ptr"
)

type accountStub struct {
	Service
	key string
	err error
}

func (s accountStub) CreateContribAccount(context.Context) (string, error) { return s.key, s.err }

func TestCreateAccount(t *testing.T) {
	saved := ""
	save := func(key string) error { saved = key; return nil }
	if f := CreateAccount(context.Background(), accountStub{key: "cafe"}, save); f != nil || saved != "cafe" {
		t.Errorf("created: failure %+v, saved %q", f, saved)
	}
	for name, tc := range map[string]struct {
		err  error
		want AccountFailureKind
	}{
		"velocity": {&ptr.ServerError{Status: 400, Message: "no"}, AccountVelocity},
		"exists":   {ptr.ErrAccountExists, AccountExists},
		"other":    {errors.New("boom"), AccountRepository},
	} {
		f := CreateAccount(context.Background(), accountStub{err: tc.err}, save)
		if f == nil || f.Kind != tc.want {
			t.Errorf("%s: failure = %+v, want kind %d", name, f, tc.want)
		}
	}
	f := CreateAccount(context.Background(), accountStub{key: "cafe"}, func(string) error { return errors.New("disk full") })
	if f == nil || f.Kind != AccountNotSaved {
		t.Errorf("a failed save = %+v, want it named", f)
	}
}
