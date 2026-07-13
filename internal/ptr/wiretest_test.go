package ptr

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"testing"
)

// deflate builds the wire form of one object: zlib(JSON v). The tests use it as
// the parser's mirror - build a value, parse it back, compare - and later
// phases reuse these builders to synthesize a fixture repository.
func deflate(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("deflate: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("deflate close: %v", err)
	}
	return buf.Bytes()
}

// buildDefinitions builds a DefinitionsUpdate blob from id->value maps.
func buildDefinitions(t *testing.T, hashes, tags map[uint64]string) []byte {
	t.Helper()
	var blocks []any
	if len(hashes) > 0 {
		blocks = append(blocks, []any{defTypeHashes, idValueRows(hashes)})
	}
	if len(tags) > 0 {
		blocks = append(blocks, []any{defTypeTags, idValueRows(tags)})
	}
	return deflate(t, []any{typeDefinitionsUpdate, 1, blocks})
}

func idValueRows(m map[uint64]string) []any {
	rows := make([]any, 0, len(m))
	for id, v := range m {
		rows = append(rows, []any{id, v})
	}
	return rows
}

// contentBuilder assembles a ContentUpdate blob one action block at a time.
type contentBuilder struct {
	blocks map[int]map[int][]any // content type -> action -> rows
}

func newContent() *contentBuilder { return &contentBuilder{blocks: map[int]map[int][]any{}} }

func (b *contentBuilder) row(contentType, action int, row any) *contentBuilder {
	if b.blocks[contentType] == nil {
		b.blocks[contentType] = map[int][]any{}
	}
	b.blocks[contentType][action] = append(b.blocks[contentType][action], row)
	return b
}

func (b *contentBuilder) mappingAdd(tagID uint64, hashIDs ...uint64) *contentBuilder {
	return b.row(contentTypeMappings, actionAdd, []any{tagID, hashIDs})
}
func (b *contentBuilder) mappingDel(tagID uint64, hashIDs ...uint64) *contentBuilder {
	return b.row(contentTypeMappings, actionDelete, []any{tagID, hashIDs})
}
func (b *contentBuilder) siblingAdd(bad, good uint64) *contentBuilder {
	return b.row(contentTypeSiblings, actionAdd, []any{bad, good})
}
func (b *contentBuilder) parentAdd(child, parent uint64) *contentBuilder {
	return b.row(contentTypeParents, actionAdd, []any{child, parent})
}

func (b *contentBuilder) build(t *testing.T) []byte {
	t.Helper()
	var blocks []any
	for ct, actions := range b.blocks {
		var ab []any
		for action, rows := range actions {
			ab = append(ab, []any{action, rows})
		}
		blocks = append(blocks, []any{ct, ab})
	}
	return deflate(t, []any{typeContentUpdate, 1, blocks})
}

// buildMetadata builds a GET /metadata response: a SerialisableDictionary
// (type 21) wrapping the Metadata object (type 37) under "metadata_slice".
func buildMetadata(t *testing.T, nextDue int64, entries []UpdateEntry) []byte {
	t.Helper()
	raw, err := encodeMetadata(nextDue, entries)
	if err != nil {
		t.Fatalf("encodeMetadata: %v", err)
	}
	return raw
}

// encodeMetadata is buildMetadata without a *testing.T, so the fixture server
// can encode a per-request (since-filtered) manifest.
func encodeMetadata(nextDue int64, entries []UpdateEntry) ([]byte, error) {
	rows := make([]any, len(entries))
	for i, e := range entries {
		rows[i] = []any{e.Index, e.Hashes, e.Begin, e.End}
	}
	metaObj := []any{typeMetadata, 1, []any{rows, nextDue}}
	dictPayload := []any{
		[]any{
			[]any{0, "metadata_slice"}, // meta-type 0: plain JSON string
			[]any{2, metaObj},          // meta-type 2: nested serialisable
		},
	}
	data, err := json.Marshal([]any{typeDictionary, 2, dictPayload})
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
