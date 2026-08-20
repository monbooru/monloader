package ptr

import (
	"bytes"
	"compress/zlib"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// The write half of the hydrus wire format. The read side (wire.go)
// only decodes server-built update blobs; contributing means building
// the client-to-server shapes ourselves: a ClientToServerUpdate (type
// 40) of Content entries (type 23) posted as a SerialisableDictionary
// (type 21) body, plus parsers for the account-management responses,
// which arrive as zlib(JSON [21, 2, entries]) dictionaries whose values
// are meta-tuples: [0, plain], [1, hex-encoded bytes], or
// [2, [type, version, payload]].

// Serialisable type codes and C2S actions used by the write path.
const (
	typeContent     = 23
	typeList        = 26
	typeC2SUpdate   = 40
	typeTagFilter   = 44
	typeAccountType = 102
	// ActionPend and ActionPetition are the two C2S actions clients use:
	// pend adds, petition removals. Exported for the commit engine.
	ActionPend       = 2
	ActionPetition   = 4
	metaPlain        = 0
	metaBytes        = 1
	metaSerialisable = 2
	permissionCreate = 1
)

// deflateJSON is the encoder mirror of inflate: zlib(JSON(v)).
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

// c2sEntry is one content+reason pair under an action.
type c2sEntry struct {
	action  int
	content []any // [23, 1, [content_type, data]]
	reason  string
}

// C2SUpdate accumulates contribution entries. Encode preserves the
// order actions were first added in (petitions before pends is the
// caller's job) and, within an action, insertion order.
type C2SUpdate struct {
	entries []c2sEntry
}

func content(contentType int, data any) []any {
	return []any{typeContent, 1, []any{contentType, data}}
}

// AddMappings adds a mapping pend or petition: one tag onto hashes.
func (u *C2SUpdate) AddMappings(action int, tag string, hashes [][]byte, reason string) {
	hexes := make([]any, len(hashes))
	for i, h := range hashes {
		hexes[i] = hex.EncodeToString(h)
	}
	u.entries = append(u.entries, c2sEntry{action, content(contentTypeMappings, []any{tag, hexes}), reason})
}

// AddSibling adds a sibling (alias) pend or petition: bad -> good.
func (u *C2SUpdate) AddSibling(action int, bad, good, reason string) {
	u.entries = append(u.entries, c2sEntry{action, content(contentTypeSiblings, []any{bad, good}), reason})
}

// AddParent adds a parent (implication) pend or petition: child -> parent.
func (u *C2SUpdate) AddParent(action int, child, parent, reason string) {
	u.entries = append(u.entries, c2sEntry{action, content(contentTypeParents, []any{child, parent}), reason})
}

// Empty reports whether nothing has been added.
func (u *C2SUpdate) Empty() bool { return len(u.entries) == 0 }

// Len is the number of content entries added.
func (u *C2SUpdate) Len() int { return len(u.entries) }

// EncodePOSTBody renders the update as the POST /update body:
// zlib(JSON [21, 2, [[[0, "client_to_server_update"], [2, C2S]]]])
// with C2S = [40, 1, [[action, [[content, reason], ...]], ...]].
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

// ServerError is a hydrus JSON error body with its HTTP status. 400 on
// the registration endpoint means "no accounts available right now"
// (retry later); 401/403 are auth or permission refusals; 509 is a
// bandwidth refusal, retryable like the read side's.
type ServerError struct {
	Status        int
	Message       string
	ExceptionType string
}

func (e *ServerError) Error() string {
	return fmt.Sprintf("server answered %d (%s): %s", e.Status, e.ExceptionType, e.Message)
}

// Retryable reports whether the refusal is temporary.
func (e *ServerError) Retryable() bool {
	return e.Status == 429 || e.Status >= 500
}

// ParseServerError decodes a non-200 hydrus response body (plain JSON,
// not zlib). A body that does not parse still yields a usable error.
func ParseServerError(status int, body []byte) *ServerError {
	var raw struct {
		Error         string `json:"error"`
		ExceptionType string `json:"exception_type"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.Error == "" {
		return &ServerError{Status: status, Message: string(body)}
	}
	return &ServerError{Status: status, Message: raw.Error, ExceptionType: raw.ExceptionType}
}

// parseResponseDict inflates a zlib(JSON [21, 2, entries]) response and
// returns its top-level keys with meta-tuples resolved one level: plain
// values as-is, hex-bytes as the hex string, serialisables as their
// undecoded [type, version, payload] tuple.
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

// envelopePayload checks a [type, version, payload] tuple's type and
// returns its payload.
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

// metaValue resolves one meta-tuple [metatype, value].
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
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return int(i), err
	case float64:
		return int(n), nil
	}
	return 0, fmt.Errorf("not a number: %T", v)
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

// AccountType is one row of /auto_create_account_types: tuple
// [102, 2, [key_hex, title, [[content_type, action], ...],
// bandwidth_rules, [num_per_period, period_s], history]].
type AccountType struct {
	Key         string
	Title       string
	Permissions map[int]int // content type -> permission action
	VelocityNum int         // accounts creatable per period; 0 = not auto-creatable
	VelocitySec int
}

// CanCreateMappings reports the create permission on mappings - the
// one POST /update needs for a tag add to apply.
func (a AccountType) CanCreateMappings() bool {
	return a.Permissions[contentTypeMappings] >= permissionCreate
}

// CanPetition reports whether any content type carries at least
// petition permission, the gate POST /update checks.
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

// ParseAccountTypes decodes GET /auto_create_account_types.
func ParseAccountTypes(body []byte) ([]AccountType, error) {
	dict, err := parseResponseDict(body)
	if err != nil {
		return nil, err
	}
	listTuple, ok := dict["account_types"].([]any)
	if !ok {
		return nil, fmt.Errorf("response carries no account_types")
	}
	payload, err := envelopePayload(listTuple, typeList)
	if err != nil {
		return nil, err
	}
	metas, ok := payload.([]any)
	if !ok {
		return nil, fmt.Errorf("account_types payload is not a list")
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

// ParseKeyResponse extracts a hex key field (registration_key,
// access_key) from a response dictionary.
func ParseKeyResponse(body []byte, field string) (string, error) {
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

// Account is the GET /account view: the account's key and type, its
// creation time, and the janitor-set state.
type Account struct {
	Key     string
	Type    AccountType
	Created int64
	Expires int64 // 0 = never
	Banned  bool
	// BanReason is the janitor's text, verbatim, when Banned.
	BanReason string
	// Message is a janitor-set note shown to the account holder.
	Message string
}

// ParseAccountResponse decodes GET /account. The account tuple is
// [key_hex, account_type_tuple, created, expires, dictionary_string]
// where dictionary_string is an embedded JSON [21, 2, entries] carrying
// banned_info (null or [reason, created, expires]) and message.
func ParseAccountResponse(body []byte) (*Account, error) {
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

// TagFilter is the repository's server-side filter over pended
// mappings, fetched from GET /tag_filter: rules keyed by tag slice - an
// exact tag, a namespace ("booru:"), "" (every unnamespaced tag), or
// ":" (every namespaced tag) - with 1 = blocked, 0 = allowed. The
// server applies it silently; the commit engine applies it client-side
// first and reports the drops.
type TagFilter struct {
	rules map[string]int
}

// NewTagFilter builds a filter from a rule set, for callers that hold
// rules outside the wire form.
func NewTagFilter(rules map[string]int) *TagFilter {
	f := &TagFilter{rules: map[string]int{}}
	for k, v := range rules {
		f.rules[k] = v
	}
	return f
}

// ParseTagFilter decodes GET /tag_filter.
func ParseTagFilter(body []byte) (*TagFilter, error) {
	dict, err := parseResponseDict(body)
	if err != nil {
		return nil, err
	}
	tuple, ok := dict["tag_filter"].([]any)
	if !ok {
		return nil, fmt.Errorf("response carries no tag_filter")
	}
	payload, err := envelopePayload(tuple, typeTagFilter)
	if err != nil {
		return nil, err
	}
	entries, ok := payload.([]any)
	if !ok {
		return nil, fmt.Errorf("tag_filter payload is not a list")
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

// Blocks reports whether the filter would drop tag, and the rule slice
// that decides it. Specificity wins: an exact-tag rule beats the tag's
// namespace slice, which beats the global "" / ":" slices.
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

// DecodedUpdate is a POST /update body decoded back into its actions
// and entries - what a server-side processor sees. The fake repository
// and the commit-engine tests assert on it; the local-hydrus smoke can
// use it to eyeball an outgoing payload.
type DecodedUpdate struct {
	Actions []DecodedAction
}

// DecodedAction groups one action's content entries.
type DecodedAction struct {
	Action  int
	Entries []DecodedEntry
}

// DecodedEntry is one content+reason pair.
type DecodedEntry struct {
	ContentType int
	Data        any
	Reason      string
}

// DecodeC2SUpdate decodes an encoded POST /update body.
func DecodeC2SUpdate(body []byte) (DecodedUpdate, error) {
	dict, err := parseResponseDict(body)
	if err != nil {
		return DecodedUpdate{}, err
	}
	tuple, ok := dict["client_to_server_update"].([]any)
	if !ok {
		return DecodedUpdate{}, fmt.Errorf("no client_to_server_update entry")
	}
	payload, err := envelopePayload(tuple, typeC2SUpdate)
	if err != nil {
		return DecodedUpdate{}, err
	}
	actions, ok := payload.([]any)
	if !ok {
		return DecodedUpdate{}, fmt.Errorf("c2s payload is not a list")
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
		da := DecodedAction{Action: action}
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
			da.Entries = append(da.Entries, DecodedEntry{ContentType: ct, Data: cp[1], Reason: reason})
		}
		out.Actions = append(out.Actions, da)
	}
	return out, nil
}

// Content type codes of the decoded entries, matching the wire values.
const (
	ContentTypeMappings = contentTypeMappings
	ContentTypeSiblings = contentTypeSiblings
	ContentTypeParents  = contentTypeParents
)
