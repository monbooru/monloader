package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func tagBody(t *testing.T, req TagRequest, frames [][]byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	ctype, err := WriteTag(&body, req, frames)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, PathTag, &body)
	r.Header.Set("Content-Type", ctype)
	return r
}

func TestTagBodyRoundTrips(t *testing.T) {
	frames := [][]byte{bytes.Repeat([]byte{1}, 12), bytes.Repeat([]byte{2}, 12)}
	req := TagRequest{Models: []string{"a", "b"}, Priority: PriorityBatch, Frames: FrameSpec{Format: FormatRGB8, Size: 2, Count: 2}}
	got, gotFrames, err := ReadTag(tagBody(t, req, frames))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(req) || !slices.EqualFunc(gotFrames, frames, bytes.Equal) {
		t.Errorf("read %+v %v, want %+v %v", got, gotFrames, req, frames)
	}
}

func TestReadTagRefusesFramesThatDoNotMatchTheSpec(t *testing.T) {
	spec := FrameSpec{Format: FormatRGB8, Size: 2, Count: 2}
	for _, tc := range []struct {
		name   string
		req    TagRequest
		frames [][]byte
	}{
		{"short", TagRequest{Models: []string{"a"}, Frames: spec}, [][]byte{make([]byte, 12)}},
		{"long", TagRequest{Models: []string{"a"}, Frames: spec}, [][]byte{make([]byte, 12), make([]byte, 13)}},
		{"png", TagRequest{Models: []string{"a"}, Frames: FrameSpec{Format: "png", Size: 2, Count: 1}}, [][]byte{make([]byte, 12)}},
		{"too many", TagRequest{Models: []string{"a"}, Frames: FrameSpec{Format: FormatRGB8, Size: 1, Count: MaxFrames + 1}}, [][]byte{make([]byte, 3*(MaxFrames+1))}},
		{"too large", TagRequest{Models: []string{"a"}, Frames: FrameSpec{Format: FormatRGB8, Size: MaxSize + 1, Count: 1}}, nil},
		{"no model", TagRequest{Frames: spec}, [][]byte{make([]byte, 24)}},
	} {
		if _, _, err := ReadTag(tagBody(t, tc.req, tc.frames)); err == nil {
			t.Errorf("%s: read without an error", tc.name)
		}
	}
}

func TestSparseKeepsWhatAggregationCounts(t *testing.T) {
	got := Sparse([]float32{0.0009, 0.001, float32(math.NaN()), 0.5, 0})
	if fmt.Sprint(got.I, got.S) != "[1 3] [0.001 0.5]" {
		t.Errorf("sparse = %v %v", got.I, got.S)
	}
}

func fakeServer(t *testing.T) (*Fake, *Client) {
	t.Helper()
	f := &Fake{
		Token:  "tok",
		Info:   Info{Name: "gpu-box", Provider: "cuda", Slots: 4, Tasks: []string{TaskTag}},
		Models: []Model{{Name: "wd", Kind: KindTagger, Digest: "d1", Input: Input{Size: 2, Fill: [3]uint8{255, 255, 255}}, Labels: 2, LabelsETag: `"e1"`, Ready: true}},
		Labels: map[string]Labels{"wd": {Model: "wd", ETag: `"e1"`, CategoryScheme: "wd14_numeric", Labels: []Label{{Name: "1girl"}, {Name: "miku", CategoryID: 4}}}},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, &Client{BaseURL: srv.URL, Token: "tok"}
}

func TestClientReadsTheFake(t *testing.T) {
	f, c := fakeServer(t)
	ctx := context.Background()
	info, err := c.Info(ctx)
	if err != nil || info.Name != "gpu-box" || info.Slots != 4 {
		t.Fatalf("info = %+v, %v", info, err)
	}
	models, err := c.Models(ctx)
	if err != nil || len(models) != 1 || models[0].Input.Size != 2 {
		t.Fatalf("models = %+v, %v", models, err)
	}
	labels, changed, err := c.Labels(ctx, "wd", "")
	if err != nil || !changed || len(labels.Labels) != 2 || labels.Labels[1].CategoryID != 4 {
		t.Fatalf("labels = %+v, %v, %v", labels, changed, err)
	}
	if _, changed, err := c.Labels(ctx, "wd", `"e1"`); err != nil || changed {
		t.Errorf("labels with a current etag: changed %v, %v", changed, err)
	}
	resp, err := c.Tag(ctx, TagRequest{Models: []string{"wd"}, Priority: PriorityInteractive, Frames: FrameSpec{Size: 2}}, [][]byte{make([]byte, 12), make([]byte, 12)})
	if err != nil || len(resp.Results[0].Frames) != 2 {
		t.Fatalf("tag = %+v, %v", resp, err)
	}
	if call := f.Calls[0]; call.Frames != (FrameSpec{Format: FormatRGB8, Size: 2, Count: 2}) || call.Priority != PriorityInteractive {
		t.Errorf("the worker got %+v", call)
	}
}

func TestClientTellsARefusalFromATransportFailure(t *testing.T) {
	_, c := fakeServer(t)
	ctx := context.Background()
	_, err := c.Tag(ctx, TagRequest{Models: []string{"nope"}, Frames: FrameSpec{Size: 2}}, [][]byte{make([]byte, 12)})
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusNotFound || se.Code != CodeUnknownModel || IsTransport(err) {
		t.Errorf("unknown model: %v", err)
	}
	c.Token = "wrong"
	if _, err := c.Info(ctx); !errors.As(err, &se) || se.Status != http.StatusUnauthorized {
		t.Errorf("bad token: %v", err)
	}

	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	c = &Client{BaseURL: gone.URL}
	if _, err := c.Info(ctx); !IsTransport(err) {
		t.Errorf("closed server: %v, want a transport failure", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Info(cancelled); IsTransport(err) {
		t.Errorf("cancelled call counted as a transport failure: %v", err)
	}
}
