// Package worker is the wire of monworker's /v1, with a client and a fake
// worker for tests. Frames travel as RGB8 pixels, raw or as PNG, and scores
// come back sparse, so the wire carries no tag and no file name.
package worker

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
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

	// FormatRGB8 is Size x Size x 3 bytes a frame, back to back. FormatPNG is
	// the same pixels as 8-bit PNGs, each after its length in four bytes
	// big-endian: lossless, and smaller on a slow link.
	FormatRGB8 = "rgb8"
	FormatPNG  = "png"

	// HeaderToken carries the worker's token when a gateway in front of
	// it keeps Authorization for its own key.
	HeaderToken = "X-Monworker-Token"
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
	// ModelsDir is the folder the worker reads, where a model is installed.
	ModelsDir string   `json:"models_dir,omitempty"`
	Tasks     []string `json:"tasks"`
	// Formats are the frame formats a call may use; none listed is
	// FormatRGB8 alone.
	Formats []string `json:"formats,omitempty"`
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
)

// frameBytes is the size of count frames of the spec once they are RGB8;
// 0 when the spec is not one a call may carry.
func (f FrameSpec) frameBytes() int {
	if f.Format != FormatRGB8 && f.Format != FormatPNG || f.Size < 1 || f.Size > MaxSize || f.Count < 1 || f.Count > MaxFrames {
		return 0
	}
	return f.Count * f.Size * f.Size * 3
}

// pngBound is the most bytes a PNG of size x size RGB8 pixels may take: a
// picture deflate cannot shrink grows by a filter byte a row and its
// block headers.
func pngBound(size int) int { return size*size*3 + size*size/8 + 4096 }

func encodePNG(w io.Writer, frame []byte, size int) error {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for i, j := 0, 0; i < len(frame); i, j = i+3, j+4 {
		img.Pix[j], img.Pix[j+1], img.Pix[j+2], img.Pix[j+3] = frame[i], frame[i+1], frame[i+2], 0xFF
	}
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&buf, img); err != nil {
		return err
	}
	if err := binary.Write(w, binary.BigEndian, uint32(buf.Len())); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}

func decodePNG(r io.Reader, dst []byte, size int) error {
	var n uint32
	if err := binary.Read(r, binary.BigEndian, &n); err != nil {
		return errors.New("a frame's length is missing")
	}
	if n == 0 || int(n) > pngBound(size) {
		return fmt.Errorf("a frame of %d bytes, over what %d px may take", n, size)
	}
	raw := make([]byte, n)
	if _, err := io.ReadFull(r, raw); err != nil {
		return fmt.Errorf("a frame cut short of its %d bytes", n)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("a frame that is no PNG: %w", err)
	}
	if cfg.Width != size || cfg.Height != size {
		return fmt.Errorf("a frame of %dx%d, not %d px", cfg.Width, cfg.Height, size)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("a frame that is no PNG: %w", err)
	}
	switch m := img.(type) {
	case *image.RGBA:
		for i, j := 0, 0; i < len(dst); i, j = i+3, j+4 {
			dst[i], dst[i+1], dst[i+2] = m.Pix[j], m.Pix[j+1], m.Pix[j+2]
		}
	case *image.NRGBA:
		for i, j := 0, 0; i < len(dst); i, j = i+3, j+4 {
			dst[i], dst[i+1], dst[i+2] = m.Pix[j], m.Pix[j+1], m.Pix[j+2]
		}
	default:
		return errors.New("a frame that is not 8-bit RGB")
	}
	return nil
}

// BearerToken is the worker's token a call carries: HeaderToken when a
// gateway took Authorization, else the bearer.
func BearerToken(r *http.Request) string {
	if tok := strings.TrimSpace(r.Header.Get(HeaderToken)); tok != "" {
		return tok
	}
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return strings.TrimSpace(tok)
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
		if req.Frames.Format == FormatPNG {
			err = encodePNG(pw, f, req.Frames.Size)
		} else {
			_, err = pw.Write(f)
		}
		if err != nil {
			return "", err
		}
	}
	return mw.FormDataContentType(), mw.Close()
}

// ReadTag reads a tag call's body, refusing frames that do not match the
// request's spec; the frames it returns are RGB8 whatever they came as.
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
		return req, nil, fmt.Errorf("frames: want %s or %s, 1 to %d frames of 1 to %d px", FormatRGB8, FormatPNG, MaxFrames, MaxSize)
	}
	if part, err = mr.NextPart(); err != nil || part.FormName() != PartFrames {
		return req, nil, fmt.Errorf("want the %q part second", PartFrames)
	}
	buf := make([]byte, n)
	frames := make([][]byte, req.Frames.Count)
	size := n / req.Frames.Count
	for i := range frames {
		frames[i] = buf[i*size : (i+1)*size : (i+1)*size]
	}
	if req.Frames.Format == FormatPNG {
		for i := range frames {
			if err := decodePNG(part, frames[i], req.Frames.Size); err != nil {
				return req, nil, fmt.Errorf("frames part: %w", err)
			}
		}
	} else if got, err := io.ReadFull(part, buf); err != nil {
		return req, nil, fmt.Errorf("frames part: %d bytes of %d", got, n)
	}
	if extra, _ := part.Read(make([]byte, 1)); extra > 0 {
		return req, nil, errors.New("frames part: more than the frames named")
	}
	return req, frames, nil
}
