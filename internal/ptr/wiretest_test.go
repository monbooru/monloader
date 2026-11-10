package ptr

import (
	"bytes"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"testing"
)

func deflate(t testing.TB, v any) []byte {
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

func buildDefinitions(t testing.TB, hashes, tags map[uint64]string) []byte {
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

type contentBuilder struct {
	blocks map[int]map[int][]any
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

func (b *contentBuilder) build(t testing.TB) []byte {
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

func buildMetadata(t testing.TB, nextDue int64, entries []updateEntry) []byte {
	t.Helper()
	raw, err := encodeMetadata(nextDue, entries)
	if err != nil {
		t.Fatalf("encodeMetadata: %v", err)
	}
	return raw
}

func encodeMetadata(nextDue int64, entries []updateEntry) ([]byte, error) {
	rows := make([]any, len(entries))
	for i, e := range entries {
		rows[i] = []any{e.Index, e.Hashes, e.Begin, e.End}
	}
	metaObj := []any{typeMetadata, 1, []any{rows, nextDue}}
	dictPayload := []any{
		[]any{
			[]any{0, "metadata_slice"},
			[]any{2, metaObj},
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

func wireBlobs(t testing.TB, ups ...*update) [][]byte {
	t.Helper()
	out := make([][]byte, len(ups))
	for i, up := range ups {
		out[i] = encodeUpdate(t, up)
	}
	return out
}

func encodeUpdate(t testing.TB, up *update) []byte {
	t.Helper()
	if up.Definitions != nil {
		return buildDefinitions(t, up.Definitions.Hashes, up.Definitions.Tags)
	}
	b := newContent()
	if up.Content != nil {
		for _, m := range up.Content.MappingsAdd {
			b.mappingAdd(m.TagID, m.HashIDs...)
		}
		for _, m := range up.Content.MappingsDel {
			b.mappingDel(m.TagID, m.HashIDs...)
		}
		for _, p := range up.Content.SiblingsAdd {
			b.siblingAdd(p.A, p.B)
		}
		for _, p := range up.Content.SiblingsDel {
			b.row(contentTypeSiblings, actionDelete, []any{p.A, p.B})
		}
		for _, p := range up.Content.ParentsAdd {
			b.parentAdd(p.A, p.B)
		}
		for _, p := range up.Content.ParentsDel {
			b.row(contentTypeParents, actionDelete, []any{p.A, p.B})
		}
	}
	return b.build(t)
}

type DecodedUpdate struct {
	Actions []decodedAction
}

type decodedAction struct {
	Action  int
	Entries []decodedEntry
}

type decodedEntry struct {
	ContentType int
	Data        any
	Reason      string
}

func DecodeC2SUpdate(body []byte) (DecodedUpdate, error) {
	actions, err := responseList(body, "client_to_server_update", typeC2SUpdate)
	if err != nil {
		return DecodedUpdate{}, err
	}
	var out DecodedUpdate
	for _, a := range actions {
		pair, ok := a.([]any)
		if !ok || len(pair) != 2 {
			return DecodedUpdate{}, fmt.Errorf("malformed action pair")
		}
		action, err := asInt(pair[0])
		if err != nil {
			return DecodedUpdate{}, err
		}
		da := decodedAction{Action: action}
		entries, ok := pair[1].([]any)
		if !ok {
			return DecodedUpdate{}, fmt.Errorf("action entries not a list")
		}
		for _, e := range entries {
			cr, ok := e.([]any)
			if !ok || len(cr) != 2 {
				return DecodedUpdate{}, fmt.Errorf("malformed content+reason")
			}
			contentTuple, ok := cr[0].([]any)
			if !ok {
				return DecodedUpdate{}, fmt.Errorf("content is not a tuple")
			}
			cPayload, err := envelopePayload(contentTuple, typeContent)
			if err != nil {
				return DecodedUpdate{}, err
			}
			cp, ok := cPayload.([]any)
			if !ok || len(cp) != 2 {
				return DecodedUpdate{}, fmt.Errorf("content payload malformed")
			}
			ct, err := asInt(cp[0])
			if err != nil {
				return DecodedUpdate{}, err
			}
			reason, _ := cr[1].(string)
			da.Entries = append(da.Entries, decodedEntry{ContentType: ct, Data: cp[1], Reason: reason})
		}
		out.Actions = append(out.Actions, da)
	}
	return out, nil
}
