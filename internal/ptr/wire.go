// Package ptr syncs a Hydrus tag repository (the public PTR) into a local
// SQLite index and answers tag queries from it. The wire format is read
// from the hydrus source: hydrus/core/networking/HydrusNetwork.py and
// HydrusConstants.py.
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

const (
	typeContentUpdate     = 34
	typeDefinitionsUpdate = 36
	typeMetadata          = 37
	typeDictionary        = 21
)

const (
	defTypeHashes = 0
	defTypeTags   = 1
)

const (
	contentTypeMappings = 0
	contentTypeSiblings = 1
	contentTypeParents  = 2
	contentTypeFiles    = 3
)

// Server-generated updates carry only add and delete.
const (
	actionAdd    = 0
	actionDelete = 1
)

// The server caps an update at 50k definition or 250k content rows, a few
// MB; the cap bounds a hostile or corrupt blob.
const maxUpdateBytes = 64 << 20

type metadataSlice struct {
	Updates       []updateEntry
	NextUpdateDue int64
}

type updateEntry struct {
	Index  uint64
	Hashes []string
	Begin  int64
	End    int64
}

type definitions struct {
	Hashes map[uint64]string
	Tags   map[uint64]string
}

type mapping struct {
	TagID   uint64
	HashIDs []uint64
}

// pair is a sibling (bad -> good) or parent (child -> parent) relation.
type pair struct {
	A uint64
	B uint64
}

type contentRows struct {
	MappingsAdd []mapping
	MappingsDel []mapping
	SiblingsAdd []pair
	SiblingsDel []pair
	ParentsAdd  []pair
	ParentsDel  []pair
}

type update struct {
	Definitions *definitions
	Content     *contentRows
}

func (u *update) Rows() int64 {
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

// updateKind peeks at the type code so each replay pass skips the other
// kind without a full parse.
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

func parseMetadata(raw []byte) (*metadataSlice, error) {
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

func parseMetadataObject(obj json.RawMessage) (*metadataSlice, error) {
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
	slice := &metadataSlice{}
	for _, row := range rows {
		var e updateEntry
		if err := decodeTuple(row, "metadata row", &e.Index, &e.Hashes, &e.Begin, &e.End); err != nil {
			return nil, err
		}
		slice.Updates = append(slice.Updates, e)
	}
	if err := json.Unmarshal(parts[1], &slice.NextUpdateDue); err != nil {
		return nil, fmt.Errorf("decoding next_update_due: %w", err)
	}
	return slice, nil
}

func parseUpdate(raw []byte) (*update, error) {
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
		return &update{Definitions: defs}, nil
	case typeContentUpdate:
		content, err := parseContent(data)
		if err != nil {
			return nil, err
		}
		return &update{Content: content}, nil
	default:
		return nil, fmt.Errorf("update type %d is neither definitions (36) nor content (34)", typ)
	}
}

func parseDefinitions(data []byte) (*definitions, error) {
	payload, err := envelope(data, typeDefinitionsUpdate, 1)
	if err != nil {
		return nil, err
	}
	var blocks [][]json.RawMessage
	if err := json.Unmarshal(payload, &blocks); err != nil {
		return nil, fmt.Errorf("decoding definitions blocks: %w", err)
	}
	defs := &definitions{Hashes: map[uint64]string{}, Tags: map[uint64]string{}}
	for _, block := range blocks {
		var defType int
		var rows [][]json.RawMessage
		if err := decodeTuple(block, "definitions block", &defType, &rows); err != nil {
			return nil, err
		}
		dst := defs.Tags
		if defType == defTypeHashes {
			dst = defs.Hashes
		} else if defType != defTypeTags {
			continue // unknown definition kind: skip forward-compatibly
		}
		for _, row := range rows {
			var id uint64
			var val string
			if err := decodeTuple(row, "definition row", &id, &val); err != nil {
				return nil, err
			}
			dst[id] = val
		}
	}
	return defs, nil
}

func parseContent(data []byte) (*contentRows, error) {
	payload, err := envelope(data, typeContentUpdate, 1)
	if err != nil {
		return nil, err
	}
	var blocks [][]json.RawMessage
	if err := json.Unmarshal(payload, &blocks); err != nil {
		return nil, fmt.Errorf("decoding content blocks: %w", err)
	}
	c := &contentRows{}
	for _, block := range blocks {
		var contentType int
		var actions [][]json.RawMessage
		if err := decodeTuple(block, "content block", &contentType, &actions); err != nil {
			return nil, err
		}
		for _, ab := range actions {
			var action int
			var rows json.RawMessage
			if err := decodeTuple(ab, "content action", &action, &rows); err != nil {
				return nil, err
			}
			if err := c.addRows(contentType, action, rows); err != nil {
				return nil, err
			}
		}
	}
	return c, nil
}

// An unknown content type or action is skipped, not fatal.
func (c *contentRows) addRows(contentType, action int, rows json.RawMessage) error {
	switch contentType {
	case contentTypeMappings:
		ms, err := parseMappings(rows)
		if err != nil {
			return err
		}
		appendByAction(action, &c.MappingsAdd, &c.MappingsDel, ms)
	case contentTypeSiblings, contentTypeParents:
		ps, err := parsePairs(rows)
		if err != nil {
			return err
		}
		if contentType == contentTypeSiblings {
			appendByAction(action, &c.SiblingsAdd, &c.SiblingsDel, ps)
		} else {
			appendByAction(action, &c.ParentsAdd, &c.ParentsDel, ps)
		}
	case contentTypeFiles:
		// A file repository's rows; a tag repository should never carry them.
	}
	return nil
}

func appendByAction[T any](action int, add, del *[]T, rows []T) {
	switch action {
	case actionAdd:
		*add = append(*add, rows...)
	case actionDelete:
		*del = append(*del, rows...)
	}
}

func parseMappings(rows json.RawMessage) ([]mapping, error) {
	var raw [][]json.RawMessage
	if err := json.Unmarshal(rows, &raw); err != nil {
		return nil, fmt.Errorf("decoding mappings rows: %w", err)
	}
	out := make([]mapping, 0, len(raw))
	for _, row := range raw {
		var m mapping
		if err := decodeTuple(row, "mappings row", &m.TagID, &m.HashIDs); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

func decodeTuple(parts []json.RawMessage, what string, dsts ...any) error {
	if len(parts) != len(dsts) {
		return fmt.Errorf("%s has %d parts, want %d", what, len(parts), len(dsts))
	}
	for i, dst := range dsts {
		if err := json.Unmarshal(parts[i], dst); err != nil {
			return fmt.Errorf("decoding %s part %d: %w", what, i, err)
		}
	}
	return nil
}

func parsePairs(rows json.RawMessage) ([]pair, error) {
	var raw [][]uint64
	if err := json.Unmarshal(rows, &raw); err != nil {
		return nil, fmt.Errorf("decoding id-pair rows: %w", err)
	}
	out := make([]pair, 0, len(raw))
	for _, r := range raw {
		if len(r) != 2 {
			return nil, fmt.Errorf("id-pair row has %d fields, want 2", len(r))
		}
		out = append(out, pair{A: r[0], B: r[1]})
	}
	return out, nil
}
