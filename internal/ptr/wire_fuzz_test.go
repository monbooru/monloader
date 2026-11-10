package ptr

import (
	"testing"
)

func FuzzParseUpdate(f *testing.F) {
	f.Add([]byte(nil))
	f.Add([]byte("not zlib"))
	f.Add(buildDefinitions(f, map[uint64]string{1: "abcd", 9000000000: "ef01"}, map[uint64]string{2: "blue_sky"}))
	f.Add(newContent().mappingAdd(2, 1, 5, 9).siblingAdd(3, 4).parentAdd(5, 6).build(f))
	f.Add(deflate(f, []any{typeDefinitionsUpdate, 1, []any{}}))
	f.Add(deflate(f, []any{99, 1, []any{}}))
	f.Add(deflate(f, "wrong shape"))
	truncated := buildMetadata(f, 42, nil)
	f.Add(truncated[:len(truncated)/2])
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = parseUpdate(raw)
	})
}

func FuzzParseMetadata(f *testing.F) {
	f.Add([]byte(nil))
	f.Add([]byte("not zlib"))
	f.Add(buildMetadata(f, 1234567890, []updateEntry{
		{Index: 0, Hashes: []string{"aa", "bb"}, Begin: 100, End: 200},
	}))
	f.Add(buildMetadata(f, 42, nil))
	f.Add(deflate(f, []any{typeDictionary, 2, []any{}}))
	f.Add(deflate(f, []any{typeMetadata, 1, []any{}}))
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = parseMetadata(raw)
	})
}
