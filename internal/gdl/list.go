package gdl

import (
	"bufio"
	"bytes"
	"context"
	"strings"

	"github.com/monbooru/monloader/internal/logx"
	"github.com/monbooru/monloader/internal/queue"
)

// ListExtractors runs `gallery-dl --list-extractors` and parses the blocks into
// the supported-site list. Instance extractors (danbooru, gelbooru, ...) print a
// blank category, which is preserved.
func (t *Tool) ListExtractors(ctx context.Context) ([]Extractor, error) {
	args := append(t.configArgs(), "--list-extractors")
	res, err := t.run(ctx, args...)
	if err != nil {
		return nil, &queue.CodedError{Code: queue.ErrCodeDownloadFailed, Msg: err.Error()}
	}
	if res.exitCode != 0 {
		return nil, classifyError(res.exitCode, res.stderr)
	}
	return parseExtractors(res.stdout), nil
}

// parseExtractors splits the listing into blocks and pulls Category /
// Subcategory / Example from each. A block counts once any of those is set, so
// an instance extractor's blank category is not dropped.
func parseExtractors(data []byte) []Extractor {
	var out []Extractor
	var cur Extractor
	flush := func() {
		if cur.Category != "" || cur.Subcategory != "" || cur.Example != "" {
			out = append(out, cur)
		}
		cur = Extractor{}
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			flush()
			continue
		}
		key, val, ok := splitField(line)
		if !ok {
			// A non field line starts a new block (the extractor class name).
			flush()
			continue
		}
		switch strings.ToLower(key) {
		case "category":
			cat, sub := splitCategoryLine(val)
			cur.Category = cat
			if sub != "" {
				cur.Subcategory = sub
			}
		case "subcategory":
			cur.Subcategory = val
		case "example":
			cur.Example = val
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		logx.Warnf("gdl: reading extractor list: %v", err)
	}
	return out
}

// splitCategoryLine parses the value of a "Category:" line. Real gallery-dl
// prints it as "<category> - Subcategory: <subcategory>" (with an empty
// category for instance extractors); older output prints the category alone.
func splitCategoryLine(val string) (category, subcategory string) {
	if i := strings.Index(strings.ToLower(val), "subcategory:"); i >= 0 {
		subcategory = strings.TrimSpace(val[i+len("subcategory:"):])
		category = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(val[:i]), "-"))
		return category, subcategory
	}
	return val, ""
}

// splitField parses a "Key: value" line, tolerating optional whitespace
// before the colon ("Example : url"). Returns ok=false when there is no colon.
func splitField(line string) (key, val string, ok bool) {
	idx := strings.IndexByte(line, ':')
	if idx <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:idx])
	val = strings.TrimSpace(line[idx+1:])
	if strings.ContainsAny(key, " \t") {
		// "Key Word: value" is not a recognised field label; treat the whole
		// line as a block header (e.g. a URL with a scheme colon).
		return "", "", false
	}
	return key, val, true
}
