package ptr

import (
	"bytes"
	"compress/zlib"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
)

const (
	typeContent     = 23
	typeList        = 26
	typeC2SUpdate   = 40
	typeTagFilter   = 44
	typeAccountType = 102
	// ActionPend proposes an addition, ActionPetition a removal.
	ActionPend       = 2
	ActionPetition   = 4
	metaPlain        = 0
	metaBytes        = 1
	metaSerialisable = 2
	permissionCreate = 1
)

func deflateJSON(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw, err := zlib.NewWriterLevel(&buf, zlib.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(data); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type c2sEntry struct {
	action  int
	content []any
	reason  string
}

// C2SUpdate encodes actions in the order first added; adding petitions
// before pends, as hydrus does, is the caller's job.
type C2SUpdate struct {
	entries []c2sEntry
}

func content(contentType int, data any) []any {
	return []any{typeContent, 1, []any{contentType, data}}
}

func (u *C2SUpdate) AddMappings(action int, tag string, hashes [][]byte, reason string) {
	hexes := make([]any, len(hashes))
	for i, h := range hashes {
		hexes[i] = hex.EncodeToString(h)
	}
	u.entries = append(u.entries, c2sEntry{action, content(contentTypeMappings, []any{tag, hexes}), reason})
}

func (u *C2SUpdate) AddSibling(action int, bad, good, reason string) {
	u.entries = append(u.entries, c2sEntry{action, content(contentTypeSiblings, []any{bad, good}), reason})
}

func (u *C2SUpdate) AddParent(action int, child, parent, reason string) {
	u.entries = append(u.entries, c2sEntry{action, content(contentTypeParents, []any{child, parent}), reason})
}

func (u *C2SUpdate) Empty() bool { return len(u.entries) == 0 }

func (u *C2SUpdate) EncodePOSTBody() ([]byte, error) {
	var actionOrder []int
	grouped := map[int][]any{}
	for _, e := range u.entries {
		if _, ok := grouped[e.action]; !ok {
			actionOrder = append(actionOrder, e.action)
		}
		grouped[e.action] = append(grouped[e.action], []any{e.content, e.reason})
	}
	payload := make([]any, 0, len(actionOrder))
	for _, a := range actionOrder {
		payload = append(payload, []any{a, grouped[a]})
	}
	c2s := []any{typeC2SUpdate, 1, payload}
	body := []any{typeDictionary, 2, []any{
		[]any{[]any{metaPlain, "client_to_server_update"}, []any{metaSerialisable, c2s}},
	}}
	return deflateJSON(body)
}

type ServerError struct {
	Status        int
	Message       string
	ExceptionType string
}

func (e *ServerError) Error() string {
	// A non-hydrus address answers plain text, with no exception type.
	if e.ExceptionType == "" {
		return fmt.Sprintf("server answered %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("server answered %d (%s): %s", e.Status, e.ExceptionType, e.Message)
}

func (e *ServerError) Retryable() bool {
	return e.Status == 429 || e.Status >= 500
}

// Error bodies are plain JSON, not zlib like the success bodies.
func parseServerError(status int, body []byte) *ServerError {
	var raw struct {
		Error         string `json:"error"`
		ExceptionType string `json:"exception_type"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.Error == "" {
		return &ServerError{Status: status, Message: clip(string(body))}
	}
	return &ServerError{Status: status, Message: clip(raw.Error), ExceptionType: raw.ExceptionType}
}

// A proxy's error page or a hydrus traceback would otherwise land whole in
// every claimed item's error.
const maxServerMessage = 300

func clip(msg string) string {
	if len(msg) <= maxServerMessage {
		return msg
	}
	return strings.ToValidUTF8(msg[:maxServerMessage], "") + "..."
}

// parseResponseDict resolves meta-tuples one level: bytes stay hex
// strings and serialisables stay [type, version, payload] tuples.
func parseResponseDict(data []byte) (map[string]any, error) {
	raw, err := inflate(data)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tuple []any
	if err := dec.Decode(&tuple); err != nil {
		return nil, fmt.Errorf("response is not a JSON tuple: %w", err)
	}
	payload, err := envelopePayload(tuple, typeDictionary)
	if err != nil {
		return nil, err
	}
	entries, ok := payload.([]any)
	if !ok {
		return nil, fmt.Errorf("dictionary payload is not a list")
	}
	out := make(map[string]any, len(entries))
	for _, e := range entries {
		pair, ok := e.([]any)
		if !ok || len(pair) != 2 {
			return nil, fmt.Errorf("malformed dictionary entry")
		}
		key, err := metaValue(pair[0])
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok {
			continue // non-string keys carry nothing we read
		}
		val, err := metaValue(pair[1])
		if err != nil {
			return nil, err
		}
		out[name] = val
	}
	return out, nil
}

func envelopePayload(tuple []any, wantType int) (any, error) {
	if len(tuple) != 3 {
		return nil, fmt.Errorf("envelope has %d elements, want 3", len(tuple))
	}
	typ, err := asInt(tuple[0])
	if err != nil {
		return nil, fmt.Errorf("envelope type: %w", err)
	}
	if typ != wantType {
		return nil, fmt.Errorf("envelope type %d, want %d", typ, wantType)
	}
	return tuple[2], nil
}

func metaValue(v any) (any, error) {
	pair, ok := v.([]any)
	if !ok || len(pair) != 2 {
		return nil, fmt.Errorf("malformed meta tuple")
	}
	mt, err := asInt(pair[0])
	if err != nil {
		return nil, fmt.Errorf("meta type: %w", err)
	}
	switch mt {
	case metaPlain, metaBytes, metaSerialisable:
		return pair[1], nil
	default:
		return nil, fmt.Errorf("unknown meta type %d", mt)
	}
}

func asInt(v any) (int, error) {
	n, err := asInt64(v)
	return int(n), err
}

func asInt64(v any) (int64, error) {
	switch n := v.(type) {
	case json.Number:
		return n.Int64()
	case float64:
		return int64(n), nil
	}
	return 0, fmt.Errorf("not a number: %T", v)
}

// AccountType's wire payload is [key_hex, title, permissions,
// bandwidth_rules, velocity, history].
type AccountType struct {
	Key         string
	Title       string
	Permissions map[int]int // content type -> permission action
	VelocityNum int         // accounts creatable per period; 0 = not auto-creatable
	VelocitySec int
}

func (a AccountType) CanCreateMappings() bool {
	return a.Permissions[contentTypeMappings] >= permissionCreate
}

// CanPetition needs no level check: petition is the lowest permission (0),
// so any listed content type grants it.
func (a AccountType) CanPetition() bool {
	return len(a.Permissions) > 0
}

func parseAccountTypeTuple(v any) (AccountType, error) {
	tuple, ok := v.([]any)
	if !ok {
		return AccountType{}, fmt.Errorf("account type is not a tuple")
	}
	payload, err := envelopePayload(tuple, typeAccountType)
	if err != nil {
		return AccountType{}, err
	}
	fields, ok := payload.([]any)
	if !ok || len(fields) < 5 {
		return AccountType{}, fmt.Errorf("account type payload has %d fields, want >= 5", len(fields))
	}
	at := AccountType{Permissions: map[int]int{}}
	if at.Key, ok = fields[0].(string); !ok {
		return AccountType{}, fmt.Errorf("account type key is not a string")
	}
	if at.Title, ok = fields[1].(string); !ok {
		return AccountType{}, fmt.Errorf("account type title is not a string")
	}
	perms, ok := fields[2].([]any)
	if !ok {
		return AccountType{}, fmt.Errorf("account type permissions is not a list")
	}
	for _, p := range perms {
		pair, ok := p.([]any)
		if !ok || len(pair) != 2 {
			return AccountType{}, fmt.Errorf("malformed permission pair")
		}
		ct, err := asInt(pair[0])
		if err != nil {
			return AccountType{}, err
		}
		action, err := asInt(pair[1])
		if err != nil {
			return AccountType{}, err
		}
		at.Permissions[ct] = action
	}
	if vel, ok := fields[4].([]any); ok && len(vel) == 2 {
		if at.VelocityNum, err = asInt(vel[0]); err != nil {
			return AccountType{}, err
		}
		if at.VelocitySec, err = asInt(vel[1]); err != nil {
			return AccountType{}, err
		}
	}
	return at, nil
}

// responseList unwraps a response dict's key from its serialisable
// envelope into the list it carries.
func responseList(body []byte, key string, wantType int) ([]any, error) {
	dict, err := parseResponseDict(body)
	if err != nil {
		return nil, err
	}
	tuple, ok := dict[key].([]any)
	if !ok {
		return nil, fmt.Errorf("response carries no %s", key)
	}
	payload, err := envelopePayload(tuple, wantType)
	if err != nil {
		return nil, err
	}
	list, ok := payload.([]any)
	if !ok {
		return nil, fmt.Errorf("%s payload is not a list", key)
	}
	return list, nil
}

func parseAccountTypes(body []byte) ([]AccountType, error) {
	metas, err := responseList(body, "account_types", typeList)
	if err != nil {
		return nil, err
	}
	out := make([]AccountType, 0, len(metas))
	for _, m := range metas {
		v, err := metaValue(m)
		if err != nil {
			return nil, err
		}
		at, err := parseAccountTypeTuple(v)
		if err != nil {
			return nil, err
		}
		out = append(out, at)
	}
	return out, nil
}

func parseKeyResponse(body []byte, field string) (string, error) {
	dict, err := parseResponseDict(body)
	if err != nil {
		return "", err
	}
	key, ok := dict[field].(string)
	if !ok || key == "" {
		return "", fmt.Errorf("response carries no %s", field)
	}
	if _, err := hex.DecodeString(key); err != nil {
		return "", fmt.Errorf("%s is not hex: %w", field, err)
	}
	return key, nil
}

type Account struct {
	Key       string
	Type      AccountType
	Created   int64
	Expires   int64 // 0 = never
	Banned    bool
	BanReason string
	Message   string
}

// The account tuple's fifth field is a JSON string holding a second
// dictionary, with banned_info and message.
func parseAccountResponse(body []byte) (*Account, error) {
	dict, err := parseResponseDict(body)
	if err != nil {
		return nil, err
	}
	tuple, ok := dict["account"].([]any)
	if !ok || len(tuple) != 5 {
		return nil, fmt.Errorf("response carries no account tuple")
	}
	acc := &Account{}
	if acc.Key, ok = tuple[0].(string); !ok {
		return nil, fmt.Errorf("account key is not a string")
	}
	if acc.Type, err = parseAccountTypeTuple(tuple[1]); err != nil {
		return nil, err
	}
	if acc.Created, err = asInt64(tuple[2]); err != nil {
		return nil, fmt.Errorf("account created: %w", err)
	}
	if tuple[3] != nil {
		if acc.Expires, err = asInt64(tuple[3]); err != nil {
			return nil, fmt.Errorf("account expires: %w", err)
		}
	}
	dictString, ok := tuple[4].(string)
	if !ok {
		return nil, fmt.Errorf("account dictionary is not a string")
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(dictString)))
	dec.UseNumber()
	var inner []any
	if err := dec.Decode(&inner); err != nil {
		return nil, fmt.Errorf("account dictionary: %w", err)
	}
	payload, err := envelopePayload(inner, typeDictionary)
	if err != nil {
		return nil, err
	}
	entries, _ := payload.([]any)
	for _, e := range entries {
		pair, ok := e.([]any)
		if !ok || len(pair) != 2 {
			continue
		}
		key, err := metaValue(pair[0])
		if err != nil {
			continue
		}
		val, err := metaValue(pair[1])
		if err != nil {
			continue
		}
		switch key {
		case "banned_info":
			if info, ok := val.([]any); ok && len(info) >= 1 {
				acc.Banned = true
				acc.BanReason, _ = info[0].(string)
			}
		case "message":
			acc.Message, _ = val.(string)
		}
	}
	return acc, nil
}

// TagFilter is applied before a send because the server drops blocked
// pends silently.
type TagFilter struct {
	rules map[string]int
}

// NewTagFilter builds a filter from explicit rules rather than a wire body.
func NewTagFilter(rules map[string]int) *TagFilter {
	return &TagFilter{rules: maps.Clone(rules)}
}

func parseTagFilter(body []byte) (*TagFilter, error) {
	entries, err := responseList(body, "tag_filter", typeTagFilter)
	if err != nil {
		return nil, err
	}
	f := &TagFilter{rules: map[string]int{}}
	for _, e := range entries {
		pair, ok := e.([]any)
		if !ok || len(pair) != 2 {
			return nil, fmt.Errorf("malformed tag_filter rule")
		}
		slice, ok := pair[0].(string)
		if !ok {
			return nil, fmt.Errorf("tag_filter slice is not a string")
		}
		rule, err := asInt(pair[1])
		if err != nil {
			return nil, err
		}
		f.rules[slice] = rule
	}
	return f, nil
}

func (f *TagFilter) Blocks(tag string) (bool, string) {
	if f == nil || len(f.rules) == 0 {
		return false, ""
	}
	if rule, ok := f.rules[tag]; ok {
		return rule == 1, tag
	}
	ns, _, hasNS := strings.Cut(tag, ":")
	if hasNS {
		if rule, ok := f.rules[ns+":"]; ok {
			return rule == 1, ns + ":"
		}
		if rule, ok := f.rules[":"]; ok {
			return rule == 1, ":"
		}
		return false, ""
	}
	if rule, ok := f.rules[""]; ok {
		return rule == 1, ""
	}
	return false, ""
}

// DecodedUpdate and DecodeC2SUpdate read an encoded update back the way the
// server would.
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

const ContentTypeMappings = contentTypeMappings
