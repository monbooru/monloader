// Package worker spells what monbooru and monworker exchange over
// monworker's /v1: the descriptions, the tag call, a client and a fake
// worker for tests. A model's frames travel as raw RGB8 and its scores
// come back sparse, so the wire carries no tag and no file name.
package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
)

const (
	PathInfo    = "/v1/info"
	PathModels  = "/v1/models"
	PathCatalog = "/v1/catalog"
	PathTag     = "/v1/tag"
)

// PathLabels is a model's labels, in output order.
func PathLabels(model string) string { return PathModels + "/" + model + "/labels" }

const (
	TaskTag    = "tag"
	KindTagger = "tagger"

	PriorityBatch       = "batch"
	PriorityInteractive = "interactive"

	FormatRGB8 = "rgb8"
)

// ScoreFloor is the lowest score a worker sends: aggregation counts
// nothing under it.
const ScoreFloor = 0.001

// A call carries at most MaxFrames frames of at most MaxSize pixels a side.
const (
	MaxFrames = 64
	MaxSize   = 2048
)

const (
	PartRequest = "request"
	PartFrames  = "frames"
)

// Info is what a worker says of itself.
type Info struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// Provider is the one in use; Providers the ones that load here.
	Provider  string   `json:"provider"`
	Providers []string `json:"providers"`
	Threads   int      `json:"threads"`
	Slots     int      `json:"slots"`
	Device    string   `json:"device,omitempty"`
	Tasks     []string `json:"tasks"`
	// Loaded names the models with a live session.
	Loaded []string `json:"loaded"`
	Busy   bool     `json:"busy"`
}

// Model is one folder of the worker's models directory.
type Model struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Digest covers the weights, the label file and the resolved profile:
	// two workers hold the same model when name and digest match.
	Digest    string `json:"digest,omitempty"`
	ModelFile string `json:"model_file,omitempty"`
	LabelFile string `json:"label_file,omitempty"`
	Input     Input  `json:"input"`
	Labels    int    `json:"labels"`
	// LabelsETag changes whenever PathLabels would answer differently.
	LabelsETag string `json:"labels_etag,omitempty"`
	// Profile is the resolved profile; Sidecar names the axes the
	// folder's own tagger.json sets.
	Profile json.RawMessage `json:"profile,omitempty"`
	Sidecar []string        `json:"sidecar,omitempty"`
	Ready   bool            `json:"ready"`
	Reason  string          `json:"reason,omitempty"`
}

// Input is the frame a model takes: Size x Size, a smaller picture
// centred on Fill.
type Input struct {
	Size int      `json:"size"`
	Fill [3]uint8 `json:"fill"`
}

type Labels struct {
	Model string `json:"model"`
	ETag  string `json:"etag"`
	// CategoryScheme and CategoryMap are the profile's, for the caller's
	// routing.
	CategoryScheme string            `json:"category_scheme"`
	CategoryMap    map[string]string `json:"category_map,omitempty"`
	Labels         []Label           `json:"labels"`
	// Mappings are the model's stock mappings, dispatch_default.json.
	Mappings []Mapping `json:"mappings,omitempty"`
}

// Label is one output channel: the name as the label file has it, "" for
// a channel it leaves out, and the category the model gives it, by name
// or by WD14 number.
type Label struct {
	Name       string `json:"name"`
	Category   string `json:"category,omitempty"`
	CategoryID int    `json:"category_id,omitempty"`
}

// Mapping routes Source into Category, renamed to Name when set; an empty
// Category drops the label.
type Mapping struct {
	Source   string `json:"source"`
	Category string `json:"category"`
	Name     string `json:"name,omitempty"`
}

// CatalogEntry is a model the worker knows how to fetch; it never fetches
// one itself.
type CatalogEntry struct {
	Name              string             `json:"name"`
	Description       string             `json:"description"`
	Files             []CatalogFile      `json:"files"`
	Gated             bool               `json:"gated,omitempty"`
	DefaultThreshold  float64            `json:"default_threshold,omitempty"`
	DefaultThresholds map[string]float64 `json:"default_thresholds,omitempty"`
	DefaultTopK       map[string]int     `json:"default_top_k,omitempty"`
	// Command fetches the missing files into the models directory, run
	// there on the worker's machine; empty when nothing is missing.
	Command string `json:"command,omitempty"`
}

type CatalogFile struct {
	URL      string `json:"url"`
	Filename string `json:"filename"`
}

type TagRequest struct {
	Models   []string  `json:"models"`
	Priority string    `json:"priority"`
	Frames   FrameSpec `json:"frames"`
}

type FrameSpec struct {
	Format string `json:"format"`
	Size   int    `json:"size"`
	Count  int    `json:"count"`
}

// TagResponse.Results follows TagRequest.Models, each with one entry per
// frame.
type TagResponse struct {
	Results []ModelScores `json:"results"`
}

type ModelScores struct {
	Model  string        `json:"model"`
	Frames []FrameScores `json:"frames"`
}

// FrameScores holds a frame's scores at or above ScoreFloor: I the label
// indexes in ascending order, S their scores. A frame the model refused
// carries Err.
type FrameScores struct {
	I   []int32   `json:"i"`
	S   []float32 `json:"s"`
	Err string    `json:"err,omitempty"`
}

// Sparse keeps the scores of dense at or above ScoreFloor; NaN fails it.
func Sparse(dense []float32) FrameScores {
	out := FrameScores{I: []int32{}, S: []float32{}}
	for i, s := range dense {
		if s >= ScoreFloor {
			out.I = append(out.I, int32(i))
			out.S = append(out.S, s)
		}
	}
	return out
}

// Error is a worker's refusal.
type Error struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

const (
	CodeUnauthorized = "unauthorized"
	CodeBadRequest   = "bad_request"
	CodeUnknownModel = "unknown_model"
	CodeNotReady     = "not_ready"
	CodeOverloaded   = "overloaded"
)

// frameBytes is the size of count frames of the spec; 0 when the spec is
// not one a call may carry.
func (f FrameSpec) frameBytes() int {
	if f.Format != FormatRGB8 || f.Size < 1 || f.Size > MaxSize || f.Count < 1 || f.Count > MaxFrames {
		return 0
	}
	return f.Count * f.Size * f.Size * 3
}

// WriteTag writes a tag call's body; it returns the content type.
func WriteTag(w io.Writer, req TagRequest, frames [][]byte) (string, error) {
	mw := multipart.NewWriter(w)
	head := textproto.MIMEHeader{}
	head.Set("Content-Disposition", `form-data; name="`+PartRequest+`"`)
	head.Set("Content-Type", "application/json")
	pw, err := mw.CreatePart(head)
	if err != nil {
		return "", err
	}
	if err := json.NewEncoder(pw).Encode(req); err != nil {
		return "", err
	}
	head = textproto.MIMEHeader{}
	head.Set("Content-Disposition", `form-data; name="`+PartFrames+`"`)
	head.Set("Content-Type", "application/octet-stream")
	if pw, err = mw.CreatePart(head); err != nil {
		return "", err
	}
	for _, f := range frames {
		if _, err := pw.Write(f); err != nil {
			return "", err
		}
	}
	return mw.FormDataContentType(), mw.Close()
}

// ReadTag reads a tag call's body, refusing frames that do not match the
// request's spec.
func ReadTag(r *http.Request) (TagRequest, [][]byte, error) {
	var req TagRequest
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		return req, nil, errors.New("want a multipart/form-data body")
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	part, err := mr.NextPart()
	if err != nil || part.FormName() != PartRequest {
		return req, nil, fmt.Errorf("want the %q part first", PartRequest)
	}
	if err := json.NewDecoder(io.LimitReader(part, 1<<20)).Decode(&req); err != nil {
		return req, nil, fmt.Errorf("request part: %w", err)
	}
	if len(req.Models) == 0 {
		return req, nil, errors.New("no model named")
	}
	n := req.Frames.frameBytes()
	if n == 0 {
		return req, nil, fmt.Errorf("frames: want %s, 1 to %d frames of 1 to %d px", FormatRGB8, MaxFrames, MaxSize)
	}
	if part, err = mr.NextPart(); err != nil || part.FormName() != PartFrames {
		return req, nil, fmt.Errorf("want the %q part second", PartFrames)
	}
	buf := make([]byte, n)
	if got, err := io.ReadFull(part, buf); err != nil {
		return req, nil, fmt.Errorf("frames part: %d bytes of %d", got, n)
	}
	if extra, _ := part.Read(make([]byte, 1)); extra > 0 {
		return req, nil, fmt.Errorf("frames part: more than %d bytes", n)
	}
	frames := make([][]byte, req.Frames.Count)
	size := n / req.Frames.Count
	for i := range frames {
		frames[i] = buf[i*size : (i+1)*size : (i+1)*size]
	}
	return req, frames, nil
}
