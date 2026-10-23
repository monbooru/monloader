package contract

type JobAccepted struct {
	JobID int64 `json:"job_id"`
}

type ImageJobRequest struct {
	ImageID int64  `json:"image_id"`
	Gallery string `json:"gallery"`
	URL     string `json:"url"`
}

type LookupRequest struct {
	ImageID    int64  `json:"image_id"`
	Gallery    string `json:"gallery"`
	Backend    string `json:"backend"`
	MD5        string `json:"md5"`
	SHA256     string `json:"sha256"`
	Background bool   `json:"background"`
	Budgeted   bool   `json:"budgeted"`
}

type Gallery struct {
	Name   string `json:"name"`
	Images int    `json:"images"`
	Tags   int    `json:"tags"`
	Active bool   `json:"active"`
}

type GalleryRename struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type PTRLookupImage struct {
	ImageID int64  `json:"image_id"`
	SHA256  string `json:"sha256"`
}

type PTRLookupRequest struct {
	Images    []PTRLookupImage `json:"images"`
	Gallery   string           `json:"gallery"`
	Scheduled bool             `json:"scheduled"`
}

type PTRLookupResponse struct {
	Index   uint64              `json:"index"`
	Results map[string][]string `json:"results"`
}

type PTRTagsRequest struct {
	Tags []string `json:"tags"`
}

type PTRTagInfo struct {
	Known        bool     `json:"known"`
	Ideal        string   `json:"ideal,omitempty"`
	Aliases      []string `json:"aliases,omitempty"`
	Implications []string `json:"implications,omitempty"`
	ImpliedBy    []string `json:"implied_by,omitempty"`
}

type PTRTagsResponse struct {
	Results map[string]PTRTagInfo `json:"results"`
}

type PTRCluster struct {
	Ideal        string   `json:"ideal"`
	Matched      []string `json:"matched"`
	Aliases      int      `json:"aliases"`
	Implications int      `json:"implications"`
	ImpliedBy    int      `json:"implied_by"`
}

type PTRSearchResponse struct {
	Clusters  []PTRCluster `json:"clusters"`
	Truncated bool         `json:"truncated"`
}

type ContribPreviewRequest struct {
	SHA256  string   `json:"sha256"`
	Tags    []string `json:"tags"`
	Implied []string `json:"implied"`
}

type ContribToAdd struct {
	Tag        string `json:"tag"`
	PTR        string `json:"ptr"`
	Status     string `json:"status"`
	Note       string `json:"note,omitempty"`
	UnknownTag bool   `json:"unknown_tag,omitempty"`
}

type ContribPTROnly struct {
	Tag          string `json:"tag"`
	PTR          string `json:"ptr"`
	Petitionable bool   `json:"petitionable"`
}

type ContribPreviewResponse struct {
	Provisional bool             `json:"provisional"`
	ToAdd       []ContribToAdd   `json:"to_add"`
	PTROnly     []ContribPTROnly `json:"ptr_only"`
}

type ContribItem struct {
	Kind   string `json:"kind"`
	SHA256 string `json:"sha256,omitempty"`
	Tag    string `json:"tag,omitempty"`
	Bad    string `json:"bad,omitempty"`
	Good   string `json:"good,omitempty"`
	Child  string `json:"child,omitempty"`
	Parent string `json:"parent,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Commit is true when absent.
type ContribRequest struct {
	Commit *bool         `json:"commit,omitempty"`
	Origin string        `json:"origin"`
	Items  []ContribItem `json:"items"`
}

type ContribResult struct {
	Kind   string `json:"kind"`
	Result string `json:"result"`
	Note   string `json:"note,omitempty"`
}

type ContribResponse struct {
	Results []ContribResult `json:"results"`
	JobID   int64           `json:"job_id,omitempty"`
}

type PairPreviewRequest struct {
	Kind string `json:"kind"`
	A    string `json:"a"`
	B    string `json:"b"`
}

type PairPreviewResponse struct {
	APtr        string `json:"a_ptr"`
	BPtr        string `json:"b_ptr"`
	Direction   string `json:"direction"`
	Note        string `json:"note,omitempty"`
	Provisional bool   `json:"provisional"`
}

// Body is the plain-text fallback for BodyHTML, which monbooru converts.
type NoteBox struct {
	X        int    `json:"x"`
	Y        int    `json:"y"`
	W        int    `json:"w"`
	H        int    `json:"h"`
	Body     string `json:"body"`
	BodyHTML string `json:"body_html,omitempty"`
}

type PostFields struct {
	Tags                 []string `json:"tags"`
	Source               string   `json:"source"`
	PostID               string   `json:"post_id,omitempty"`
	URL                  string   `json:"url"`
	Commentary           string   `json:"commentary,omitempty"`
	CommentaryTranslated string   `json:"commentary_translated,omitempty"`
	// The same two bodies in the source's markup, which monbooru converts
	// and prefers over the plain fields.
	CommentaryDText           string    `json:"commentary_dtext,omitempty"`
	CommentaryTranslatedDText string    `json:"commentary_translated_dtext,omitempty"`
	Original                  string    `json:"original,omitempty"`
	ParentURL                 string    `json:"parent_url,omitempty"`
	Notes                     []NoteBox `json:"notes,omitempty"`
	// What the post says about the file it serves, which monbooru checks
	// against the local file.
	PostWidth  int    `json:"post_width,omitempty"`
	PostHeight int    `json:"post_height,omitempty"`
	PostSize   int64  `json:"post_size,omitempty"`
	PostExt    string `json:"post_ext,omitempty"`
}

type EnrichRequest struct {
	PostFields
	SourceMD5  string  `json:"source_md5,omitempty"`
	Verify     bool    `json:"verify"`
	Similarity float64 `json:"similarity,omitempty"`
}

// On a push, Tags and Notes travel as JSON.
const (
	FieldFile                      = "file"
	FieldTags                      = "tags"
	FieldVia                       = "via"
	FieldFolder                    = "folder"
	FieldSource                    = "source"
	FieldPostID                    = "post_id"
	FieldURL                       = "url"
	FieldCommentary                = "commentary"
	FieldCommentaryTranslated      = "commentary_translated"
	FieldCommentaryDText           = "commentary_dtext"
	FieldCommentaryTranslatedDText = "commentary_translated_dtext"
	FieldOriginal                  = "original"
	FieldParentURL                 = "parent_url"
	FieldNotes                     = "notes"
	FieldCollection                = "collection"
	FieldCollectionOrder           = "collection_order"
	FieldPostWidth                 = "post_width"
	FieldPostHeight                = "post_height"
	FieldPostSize                  = "post_size"
	FieldPostExt                   = "post_ext"
	FieldMD5                       = "md5"
)
