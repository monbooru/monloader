// Package ptr implements a thin client for a Hydrus tag repository (the public
// PTR): it streams the repository's update history, replays it into a local
// SQLite index, and answers hash -> tags and tag alias / implication queries.
// This file decodes the repository wire format.
//
// Every hydrus network object is UTF-8 JSON wrapped as a positional array and
// zlib-compressed: zlib(JSON [type, version, payload]). The type codes and
// payload shapes below are the repository protocol as read from the hydrus
// source (hydrus/core/networking/HydrusNetwork.py and HydrusConstants.py); they
// have been stable at serialisable version 1 since the protocol's inception.
package ptr

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Serialisable type codes carried in an object's leading position.
const (
	typeContentUpdate     = 34
	typeDefinitionsUpdate = 36
	typeMetadata          = 37
	typeDictionary        = 21 // response envelope for /metadata
)

// Definition block kinds inside a DefinitionsUpdate.
const (
	defTypeHashes = 0
	defTypeTags   = 1
)

// Content block kinds inside a ContentUpdate. A tag repository carries only
// mappings, siblings, and parents; a files block (3) belongs to a file
// repository and is tolerated but ignored.
const (
	contentTypeMappings = 0
	contentTypeSiblings = 1
	contentTypeParents  = 2
	contentTypeFiles    = 3
)

// Content actions. Server-generated updates carry only add and delete.
const (
	actionAdd    = 0
	actionDelete = 1
)

// maxUpdateBytes caps an inflated update. The server's generator bounds an
// update to 50k definition or 250k content rows, so a real update is a few MB
// at most; the cap guards against a hostile or corrupt blob inflating without
// bound. The replay inflates one blob at a time (an entry's blobs are
// buffered compressed), so the cap bounds transient memory, not a whole
// entry.
const maxUpdateBytes = 64 << 20

// MetadataSlice is the manifest returned by GET /metadata: the update files
// available from a given index onward, and when the next update is due.
type MetadataSlice struct {
	Updates       []UpdateEntry
	NextUpdateDue int64
}

// UpdateEntry is one update index: the sha256 hashes (hex) of the compressed
// update files that make it up, and the server-side time window they cover.
type UpdateEntry struct {
	Index  uint64
	Hashes []string
	Begin  int64
	End    int64
}

// Definitions maps the service's integer ids to their meaning: hash_id to a
// file sha256 (hex), tag_id to a tag string.
type Definitions struct {
	Hashes map[uint64]string
	Tags   map[uint64]string
}

// Mapping is one mappings row: a tag applied to a block of files.
type Mapping struct {
	TagID   uint64
	HashIDs []uint64
}

// Pair is a sibling (bad -> good) or parent (child -> parent) relation.
type Pair struct {
	A uint64
	B uint64
}

// Content is one ContentUpdate's rows, split by kind and action.
type Content struct {
	MappingsAdd []Mapping
	MappingsDel []Mapping
	SiblingsAdd []Pair
	SiblingsDel []Pair
	ParentsAdd  []Pair
	ParentsDel  []Pair
}

// Update is one parsed update file: exactly one of Definitions or Content is
// set, per the leading type code.
type Update struct {
	Definitions *Definitions
	Content     *Content
}

// Rows counts the rows an update carries - definitions, mapping cells, and
// relation pairs - the unit the sync's processing rate is reported in.
func (u *Update) Rows() int64 {
	var n int64
	if u.Definitions != nil {
		n += int64(len(u.Definitions.Hashes) + len(u.Definitions.Tags))
	}
	if u.Content != nil {
		for _, m := range u.Content.MappingsAdd {
			n += int64(len(m.HashIDs))
		}
		for _, m := range u.Content.MappingsDel {
			n += int64(len(m.HashIDs))
		}
		n += int64(len(u.Content.SiblingsAdd) + len(u.Content.SiblingsDel) +
			len(u.Content.ParentsAdd) + len(u.Content.ParentsDel))
	}
	return n
}

// updateKind reads the leading type code of a compressed update without
// inflating the whole blob, so the replay's two passes (definitions before
// content) can skip the other kind's full parse.
func updateKind(raw []byte) (int, error) {
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return 0, fmt.Errorf("opening zlib stream: %w", err)
	}
	defer zr.Close()
	buf := make([]byte, 16)
	n, err := io.ReadFull(zr, buf)
	if err != nil && err != io.ErrUnexpectedEOF {
		return 0, fmt.Errorf("reading update head: %w", err)
	}
	head := strings.TrimLeft(string(buf[:n]), " \t\r\n")
	if len(head) == 0 || head[0] != '[' {
		return 0, fmt.Errorf("update does not open a wrapper array")
	}
	head = strings.TrimLeft(head[1:], " \t\r\n")
	i := 0
	for i < len(head) && head[i] >= '0' && head[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("update wrapper has no leading type code")
	}
	return strconv.Atoi(head[:i])
}

// inflate zlib-decompresses raw with the size cap applied.
func inflate(raw []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("opening zlib stream: %w", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, maxUpdateBytes+1))
	if err != nil {
		return nil, fmt.Errorf("inflating: %w", err)
	}
	if len(out) > maxUpdateBytes {
		return nil, fmt.Errorf("inflated object exceeds %d bytes", maxUpdateBytes)
	}
	return out, nil
}

// envelope decodes the [type, version, payload] wrapper, checking the type is
// wanted and the version is not newer than known.
func envelope(data []byte, wantType, knownVersion int) (json.RawMessage, error) {
	var outer []json.RawMessage
	if err := json.Unmarshal(data, &outer); err != nil {
		return nil, fmt.Errorf("decoding object wrapper: %w", err)
	}
	if len(outer) != 3 {
		return nil, fmt.Errorf("object wrapper has %d parts, want 3", len(outer))
	}
	var typ, ver int
	if err := json.Unmarshal(outer[0], &typ); err != nil {
		return nil, fmt.Errorf("decoding object type: %w", err)
	}
	if typ != wantType {
		return nil, fmt.Errorf("object type %d, want %d", typ, wantType)
	}
	if err := json.Unmarshal(outer[1], &ver); err != nil {
		return nil, fmt.Errorf("decoding object version: %w", err)
	}
	if ver > knownVersion {
		return nil, fmt.Errorf("object version %d is newer than the known %d", ver, knownVersion)
	}
	return outer[2], nil
}

// ParseMetadata decodes a GET /metadata response. The body is a
// SerialisableDictionary (type 21) wrapping one entry, "metadata_slice", whose
// value is a nested Metadata object (type 37). Each dictionary side is a
// [meta-type, value] tuple; meta-type 2 marks a nested serialisable.
func ParseMetadata(raw []byte) (*MetadataSlice, error) {
	data, err := inflate(raw)
	if err != nil {
		return nil, err
	}
	dictPayload, err := envelope(data, typeDictionary, 2)
	if err != nil {
		return nil, err
	}
	var pairs [][]json.RawMessage
	if err := json.Unmarshal(dictPayload, &pairs); err != nil {
		return nil, fmt.Errorf("decoding metadata dictionary: %w", err)
	}
	for _, pair := range pairs {
		if len(pair) != 2 {
			continue
		}
		key, err := dictString(pair[0])
		if err != nil || key != "metadata_slice" {
			continue
		}
		nested, err := dictNested(pair[1])
		if err != nil {
			return nil, err
		}
		return parseMetadataObject(nested)
	}
	return nil, fmt.Errorf("metadata response has no metadata_slice entry")
}

// dictString reads a [meta-type, value] tuple that holds a plain-JSON string
// (meta-type 0).
func dictString(raw json.RawMessage) (string, error) {
	var tuple []json.RawMessage
	if err := json.Unmarshal(raw, &tuple); err != nil || len(tuple) != 2 {
		return "", fmt.Errorf("malformed dictionary key")
	}
	var s string
	if err := json.Unmarshal(tuple[1], &s); err != nil {
		return "", err
	}
	return s, nil
}

// dictNested reads a [meta-type, value] tuple that holds a nested serialisable
// object (meta-type 2), returning the inner [type, version, payload] bytes.
func dictNested(raw json.RawMessage) (json.RawMessage, error) {
	var tuple []json.RawMessage
	if err := json.Unmarshal(raw, &tuple); err != nil || len(tuple) != 2 {
		return nil, fmt.Errorf("malformed dictionary value")
	}
	var metaType int
	if err := json.Unmarshal(tuple[0], &metaType); err != nil {
		return nil, err
	}
	if metaType != 2 {
		return nil, fmt.Errorf("metadata_slice value meta-type %d, want 2 (nested)", metaType)
	}
	return tuple[1], nil
}

// parseMetadataObject decodes the Metadata object's [type, version, payload].
// The payload is [ [[index, [hex hashes], begin, end], ...], next_update_due ].
func parseMetadataObject(obj json.RawMessage) (*MetadataSlice, error) {
	payload, err := envelope(obj, typeMetadata, 1)
	if err != nil {
		return nil, err
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(payload, &parts); err != nil || len(parts) != 2 {
		return nil, fmt.Errorf("decoding metadata payload")
	}
	var rows [][]json.RawMessage
	if err := json.Unmarshal(parts[0], &rows); err != nil {
		return nil, fmt.Errorf("decoding metadata rows: %w", err)
	}
	slice := &MetadataSlice{}
	for _, row := range rows {
		if len(row) != 4 {
			return nil, fmt.Errorf("metadata row has %d fields, want 4", len(row))
		}
		var e UpdateEntry
		if err := json.Unmarshal(row[0], &e.Index); err != nil {
			return nil, fmt.Errorf("decoding update index: %w", err)
		}
		if err := json.Unmarshal(row[1], &e.Hashes); err != nil {
			return nil, fmt.Errorf("decoding update hashes: %w", err)
		}
		if err := json.Unmarshal(row[2], &e.Begin); err != nil {
			return nil, fmt.Errorf("decoding update begin: %w", err)
		}
		if err := json.Unmarshal(row[3], &e.End); err != nil {
			return nil, fmt.Errorf("decoding update end: %w", err)
		}
		slice.Updates = append(slice.Updates, e)
	}
	if err := json.Unmarshal(parts[1], &slice.NextUpdateDue); err != nil {
		return nil, fmt.Errorf("decoding next_update_due: %w", err)
	}
	return slice, nil
}

// ParseUpdate decodes a GET /update response into either a Definitions or a
// Content update, dispatching on its leading type code.
func ParseUpdate(raw []byte) (*Update, error) {
	data, err := inflate(raw)
	if err != nil {
		return nil, err
	}
	var outer []json.RawMessage
	if err := json.Unmarshal(data, &outer); err != nil {
		return nil, fmt.Errorf("decoding update wrapper: %w", err)
	}
	if len(outer) != 3 {
		return nil, fmt.Errorf("update wrapper has %d parts, want 3", len(outer))
	}
	var typ int
	if err := json.Unmarshal(outer[0], &typ); err != nil {
		return nil, fmt.Errorf("decoding update type: %w", err)
	}
	switch typ {
	case typeDefinitionsUpdate:
		defs, err := parseDefinitions(data)
		if err != nil {
			return nil, err
		}
		return &Update{Definitions: defs}, nil
	case typeContentUpdate:
		content, err := parseContent(data)
		if err != nil {
			return nil, err
		}
		return &Update{Content: content}, nil
	default:
		return nil, fmt.Errorf("update type %d is neither definitions (36) nor content (34)", typ)
	}
}

// parseDefinitions decodes a DefinitionsUpdate. The payload is a list of
// [def-type, [[id, value], ...]] blocks; a block is present only when non-empty.
func parseDefinitions(data []byte) (*Definitions, error) {
	payload, err := envelope(data, typeDefinitionsUpdate, 1)
	if err != nil {
		return nil, err
	}
	var blocks [][]json.RawMessage
	if err := json.Unmarshal(payload, &blocks); err != nil {
		return nil, fmt.Errorf("decoding definitions blocks: %w", err)
	}
	defs := &Definitions{Hashes: map[uint64]string{}, Tags: map[uint64]string{}}
	for _, block := range blocks {
		if len(block) != 2 {
			return nil, fmt.Errorf("definitions block has %d parts, want 2", len(block))
		}
		var defType int
		if err := json.Unmarshal(block[0], &defType); err != nil {
			return nil, fmt.Errorf("decoding definition type: %w", err)
		}
		var rows [][]json.RawMessage
		if err := json.Unmarshal(block[1], &rows); err != nil {
			return nil, fmt.Errorf("decoding definition rows: %w", err)
		}
		dst := defs.Tags
		if defType == defTypeHashes {
			dst = defs.Hashes
		} else if defType != defTypeTags {
			continue // unknown definition kind: skip forward-compatibly
		}
		for _, row := range rows {
			if len(row) != 2 {
				return nil, fmt.Errorf("definition row has %d fields, want 2", len(row))
			}
			var id uint64
			if err := json.Unmarshal(row[0], &id); err != nil {
				return nil, fmt.Errorf("decoding definition id: %w", err)
			}
			var val string
			if err := json.Unmarshal(row[1], &val); err != nil {
				return nil, fmt.Errorf("decoding definition value: %w", err)
			}
			dst[id] = val
		}
	}
	return defs, nil
}

// parseContent decodes a ContentUpdate. The payload is a list of
// [content-type, [[action, [row, ...]], ...]] blocks.
func parseContent(data []byte) (*Content, error) {
	payload, err := envelope(data, typeContentUpdate, 1)
	if err != nil {
		return nil, err
	}
	var blocks [][]json.RawMessage
	if err := json.Unmarshal(payload, &blocks); err != nil {
		return nil, fmt.Errorf("decoding content blocks: %w", err)
	}
	c := &Content{}
	for _, block := range blocks {
		if len(block) != 2 {
			return nil, fmt.Errorf("content block has %d parts, want 2", len(block))
		}
		var contentType int
		if err := json.Unmarshal(block[0], &contentType); err != nil {
			return nil, fmt.Errorf("decoding content type: %w", err)
		}
		var actions [][]json.RawMessage
		if err := json.Unmarshal(block[1], &actions); err != nil {
			return nil, fmt.Errorf("decoding content actions: %w", err)
		}
		for _, ab := range actions {
			if len(ab) != 2 {
				return nil, fmt.Errorf("content action has %d parts, want 2", len(ab))
			}
			var action int
			if err := json.Unmarshal(ab[0], &action); err != nil {
				return nil, fmt.Errorf("decoding content action: %w", err)
			}
			if err := c.addRows(contentType, action, ab[1]); err != nil {
				return nil, err
			}
		}
	}
	return c, nil
}

// addRows appends one action block's rows to the right slice by content type
// and action. An unknown content type or action is skipped, not fatal.
func (c *Content) addRows(contentType, action int, rows json.RawMessage) error {
	switch contentType {
	case contentTypeMappings:
		ms, err := parseMappings(rows)
		if err != nil {
			return err
		}
		switch action {
		case actionAdd:
			c.MappingsAdd = append(c.MappingsAdd, ms...)
		case actionDelete:
			c.MappingsDel = append(c.MappingsDel, ms...)
		}
	case contentTypeSiblings:
		ps, err := parsePairs(rows)
		if err != nil {
			return err
		}
		switch action {
		case actionAdd:
			c.SiblingsAdd = append(c.SiblingsAdd, ps...)
		case actionDelete:
			c.SiblingsDel = append(c.SiblingsDel, ps...)
		}
	case contentTypeParents:
		ps, err := parsePairs(rows)
		if err != nil {
			return err
		}
		switch action {
		case actionAdd:
			c.ParentsAdd = append(c.ParentsAdd, ps...)
		case actionDelete:
			c.ParentsDel = append(c.ParentsDel, ps...)
		}
	case contentTypeFiles:
		// A file repository's rows; a tag repository should never carry them.
	}
	return nil
}

// parseMappings decodes mappings rows: [tag_id, [hash_id, ...]].
func parseMappings(rows json.RawMessage) ([]Mapping, error) {
	var raw []json.RawMessage
	if err := json.Unmarshal(rows, &raw); err != nil {
		return nil, fmt.Errorf("decoding mappings rows: %w", err)
	}
	out := make([]Mapping, 0, len(raw))
	for _, r := range raw {
		var row []json.RawMessage
		if err := json.Unmarshal(r, &row); err != nil || len(row) != 2 {
			return nil, fmt.Errorf("mappings row is not a [tag_id, hash_ids] pair")
		}
		var m Mapping
		if err := json.Unmarshal(row[0], &m.TagID); err != nil {
			return nil, fmt.Errorf("decoding mapping tag id: %w", err)
		}
		if err := json.Unmarshal(row[1], &m.HashIDs); err != nil {
			return nil, fmt.Errorf("decoding mapping hash ids: %w", err)
		}
		out = append(out, m)
	}
	return out, nil
}

// parsePairs decodes sibling / parent rows: [a_id, b_id].
func parsePairs(rows json.RawMessage) ([]Pair, error) {
	var raw [][]uint64
	if err := json.Unmarshal(rows, &raw); err != nil {
		return nil, fmt.Errorf("decoding id-pair rows: %w", err)
	}
	out := make([]Pair, 0, len(raw))
	for _, r := range raw {
		if len(r) != 2 {
			return nil, fmt.Errorf("id-pair row has %d fields, want 2", len(r))
		}
		out = append(out, Pair{A: r[0], B: r[1]})
	}
	return out, nil
}
