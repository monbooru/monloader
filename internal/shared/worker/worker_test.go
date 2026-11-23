package worker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
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
		{"unknown format", TagRequest{Models: []string{"a"}, Frames: FrameSpec{Format: "jpeg", Size: 2, Count: 1}}, [][]byte{make([]byte, 12)}},
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

func TestTagBodyRoundTripsAsPNG(t *testing.T) {
	const size = 16
	frames := make([][]byte, 3)
	for k := range frames {
		frames[k] = make([]byte, size*size*3)
		for i := range frames[k] {
			frames[k][i] = byte(i*7 + k*31)
		}
	}
	req := TagRequest{Models: []string{"a"}, Priority: PriorityBatch, Frames: FrameSpec{Format: FormatPNG, Size: size, Count: len(frames)}}
	got, gotFrames, err := ReadTag(tagBody(t, req, frames))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(req) || !slices.EqualFunc(gotFrames, frames, bytes.Equal) {
		t.Errorf("the frames did not come back as the pixels sent")
	}
	flat := [][]byte{bytes.Repeat([]byte{255}, size*size*3)}
	var raw, packed bytes.Buffer
	if _, err := WriteTag(&raw, TagRequest{Models: []string{"a"}, Frames: FrameSpec{Format: FormatRGB8, Size: size, Count: 1}}, flat); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteTag(&packed, TagRequest{Models: []string{"a"}, Frames: FrameSpec{Format: FormatPNG, Size: size, Count: 1}}, flat); err != nil {
		t.Fatal(err)
	}
	if packed.Len() >= raw.Len() {
		t.Errorf("a flat frame took %d bytes as PNG, %d raw", packed.Len(), raw.Len())
	}
}

func rawTagBody(t *testing.T, req TagRequest, payload []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	head := textproto.MIMEHeader{}
	head.Set("Content-Disposition", `form-data; name="`+PartRequest+`"`)
	pw, _ := mw.CreatePart(head)
	if err := json.NewEncoder(pw).Encode(req); err != nil {
		t.Fatal(err)
	}
	head = textproto.MIMEHeader{}
	head.Set("Content-Disposition", `form-data; name="`+PartFrames+`"`)
	pw, _ = mw.CreatePart(head)
	_, _ = pw.Write(payload)
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, PathTag, &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r
}

func TestReadTagRefusesAPNGThatIsNotTheFrameNamed(t *testing.T) {
	encode := func(img image.Image) []byte {
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	framed := func(b []byte) []byte {
		return append(binary.BigEndian.AppendUint32(nil, uint32(len(b))), b...)
	}
	good := encode(image.NewNRGBA(image.Rect(0, 0, 4, 4)))
	req := TagRequest{Models: []string{"a"}, Frames: FrameSpec{Format: FormatPNG, Size: 4, Count: 1}}
	for _, tc := range []struct {
		name    string
		payload []byte
	}{
		{"no length", nil},
		{"cut short", framed(good)[:len(good)]},
		{"not a png", framed(bytes.Repeat([]byte{9}, 40))},
		{"another size", framed(encode(image.NewNRGBA(image.Rect(0, 0, 8, 8))))},
		{"sixteen bits", framed(encode(image.NewNRGBA64(image.Rect(0, 0, 4, 4))))},
		{"a length over the bound", append(binary.BigEndian.AppendUint32(nil, uint32(pngBound(4)+1)), good...)},
		{"a second frame", append(framed(good), framed(good)...)},
	} {
		if _, _, err := ReadTag(rawTagBody(t, req, tc.payload)); err == nil {
			t.Errorf("%s: read without an error", tc.name)
		}
	}
	if _, _, err := ReadTag(rawTagBody(t, req, framed(good))); err != nil {
		t.Errorf("a well-formed frame was refused: %v", err)
	}
}

func TestClientSendsTheWorkersTokenBesideAGatewaysKey(t *testing.T) {
	var auth, own string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, own = r.Header.Get("Authorization"), BearerToken(r)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "worker-token"}
	if _, err := c.Info(context.Background()); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer worker-token" || own != "worker-token" {
		t.Errorf("without a gateway: Authorization %q, token read %q", auth, own)
	}
	c.Gateway = "gateway-key"
	if _, err := c.Info(context.Background()); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer gateway-key" || own != "worker-token" {
		t.Errorf("behind a gateway: Authorization %q, token read %q", auth, own)
	}
}
