package gdl

import (
	"bufio"
	"bytes"
	"context"
	"strings"

	"github.com/monbooru/monloader/internal/shared/logx"
)

func (t *Tool) ListExtractors(ctx context.Context) ([]Extractor, error) {
	out, err := t.runOut(ctx, append(t.configArgs(), "--list-extractors")...)
	if err != nil {
		return nil, err
	}
	return parseExtractors(out), nil
}

// Instance extractors (danbooru, gelbooru) print a blank category, so a
// block counts once any field is set.
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
			// A non-field line is the next extractor's class name.
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

// gallery-dl prints "<category> - Subcategory: <subcategory>"; older
// releases print the category alone.
func splitCategoryLine(val string) (category, subcategory string) {
	if i := strings.Index(strings.ToLower(val), "subcategory:"); i >= 0 {
		subcategory = strings.TrimSpace(val[i+len("subcategory:"):])
		category = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(val[:i]), "-"))
		return category, subcategory
	}
	return val, ""
}

func splitField(line string) (key, val string, ok bool) {
	key, val, found := strings.Cut(line, ":")
	if !found || key == "" {
		return "", "", false
	}
	key = strings.TrimSpace(key)
	if strings.ContainsAny(key, " \t") {
		// A space in the key means a header line, such as a URL with a
		// scheme colon.
		return "", "", false
	}
	return key, strings.TrimSpace(val), true
}
