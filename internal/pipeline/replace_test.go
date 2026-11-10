package pipeline

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/monbooru/monloader/internal/gdl"
	"github.com/monbooru/monloader/internal/outcome"
	"github.com/monbooru/monloader/internal/queue"
)

func fakeBytesMD5(name string) string {
	sum := md5.Sum([]byte("bytes-" + name))
	return hex.EncodeToString(sum[:])
}

type replaceCapture struct {
	mu    sync.Mutex
	paths []string
	files []string
	forms []map[string]string
}

var replaceStoredSHA = strings.Repeat("ef", 32)

func replaceStub(t *testing.T, cap *replaceCapture, fileStatus int, fileBody map[string]any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/images/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"sha256": replaceStoredSHA})
			return
		}
		cap.mu.Lock()
		defer cap.mu.Unlock()
		cap.paths = append(cap.paths, r.URL.Path+"?"+r.URL.RawQuery)
		if strings.Contains(r.URL.Path, "/file") {
			if err := r.ParseMultipartForm(16 << 20); err == nil {
				if f, _, ferr := r.FormFile("file"); ferr == nil {
					b, _ := io.ReadAll(f)
					cap.files = append(cap.files, string(b))
					_ = f.Close()
				}
				form := map[string]string{}
				for k, v := range r.MultipartForm.Value {
					if len(v) > 0 {
						form[k] = v[0]
					}
				}
				cap.forms = append(cap.forms, form)
			}
			reply(w, fileStatus, fileBody)
			return
		}
		b, _ := io.ReadAll(r.Body)
		cap.forms = append(cap.forms, map[string]string{"_body": string(b)})
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

func TestPipelineReplacePushesFile(t *testing.T) {
	post := danbooruPost("500")
	post.Meta["md5"] = fakeBytesMD5("500")
	fake := &fakeRunner{resolved: []gdl.Item{post}, writeIdx: []int{0}}
	cap := &replaceCapture{}
	q := testEnv(t, fake, replaceStub(t, cap, http.StatusOK,
		map[string]any{"replaced": true, "merge": map[string]any{"tags_added": 3}}))

	job := waitJob(t, q, q.EnqueueReplace(42, "art", "https://danbooru.donmai.us/posts/500"))

	if job.Status != queue.JobSucceeded {
		t.Errorf("status = %s, want succeeded", job.Status)
	}
	if len(job.Items) != 1 || job.Items[0].Outcome != outcome.Replaced {
		t.Fatalf("items = %+v, want one replaced", job.Items)
	}
	if job.Summary.Replaced != 1 || job.Summary.Total != 1 {
		t.Errorf("summary = %+v, want 1 replaced / 1 total", job.Summary)
	}
	if fake.gotArchive != gdl.ArchiveBypass {
		t.Error("download did not bypass the archive; a replace must always fetch")
	}
	if len(cap.paths) != 1 || cap.paths[0] != "/api/v1/images/42/file?gallery=art" {
		t.Errorf("paths = %v, want one push to /api/v1/images/42/file?gallery=art", cap.paths)
	}
	if len(cap.files) != 1 || cap.files[0] != "bytes-500" {
		t.Errorf("pushed file = %q, want the downloaded bytes", cap.files)
	}
	if len(cap.forms) != 1 || cap.forms[0]["source"] != "danbooru" ||
		!strings.Contains(cap.forms[0]["tags"], "tag_500") {
		t.Errorf("pushed form = %+v, want danbooru source and mapped tags", cap.forms)
	}
	if job.Items[0].MergeNote == "" {
		t.Error("merge note empty, want the merge summary carried onto the item")
	}
	if job.Items[0].SHA256 != replaceStoredSHA {
		t.Errorf("item sha256 = %q, want the hash monbooru stored so the link is a permalink", job.Items[0].SHA256)
	}
}

func TestPipelineReplaceHashMismatchPushesNothing(t *testing.T) {
	post := danbooruPost("501")
	post.Meta["md5"] = "00000000000000000000000000000000"
	fake := &fakeRunner{resolved: []gdl.Item{post}, writeIdx: []int{0}}
	cap := &replaceCapture{}
	q := testEnv(t, fake, replaceStub(t, cap, http.StatusOK, map[string]any{"replaced": true}))

	job := waitJob(t, q, q.EnqueueReplace(7, "", "https://danbooru.donmai.us/posts/501"))

	if len(job.Items) != 1 || job.Items[0].Outcome != outcome.Failed ||
		job.Items[0].ErrorCode != outcome.ErrCodeHashMismatch {
		t.Fatalf("items = %+v, want one failed hash_mismatch", job.Items)
	}
	for _, p := range cap.paths {
		if strings.Contains(p, "/file") {
			t.Errorf("a mismatched download was pushed: %v", cap.paths)
		}
	}
	found := false
	for _, p := range cap.paths {
		if strings.Contains(p, "/fetch-status") {
			found = true
		}
	}
	if !found {
		t.Errorf("paths = %v, want a fetch-status report", cap.paths)
	}
}

func TestPipelineReplaceRefusalKeepsMonbooruStory(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     map[string]any
		want     string
		reported bool
	}{
		{"already exists", http.StatusConflict, map[string]any{"error": "the original already exists as image 9", "code": "already_exists"}, outcome.ErrCodeAlreadyExists, false},
		{"wrong type", http.StatusConflict, map[string]any{"error": "target is an archive", "code": "wrong_type"}, outcome.ErrCodeWrongType, false},
		{"conflict", http.StatusConflict, map[string]any{"error": "the image's file is outside the gallery root", "code": "conflict"}, outcome.ErrCodeMonbooruRejected, false},
		{"too large", http.StatusRequestEntityTooLarge, map[string]any{"error": "file exceeds max size (1 MB)", "code": "file_too_large"}, outcome.ErrCodeFileTooLarge, false},
		{"does not decode", http.StatusUnsupportedMediaType, map[string]any{"error": "file does not decode as an image", "code": "unsupported_type"}, outcome.ErrCodeMonbooruRejected, false},
		{"unrecorded refusal", http.StatusNotFound, map[string]any{"error": "image not found"}, outcome.ErrCodeMonbooruRejected, true},
		{"unrecorded server error", http.StatusInternalServerError, map[string]any{"error": "failed to stage upload", "code": "internal_error"}, outcome.ErrCodeMonbooruRejected, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			post := danbooruPost("502")
			post.Meta["md5"] = fakeBytesMD5("502")
			fake := &fakeRunner{resolved: []gdl.Item{post}, writeIdx: []int{0}}
			cap := &replaceCapture{}
			q := testEnv(t, fake, replaceStub(t, cap, tc.status, tc.body))

			job := waitJob(t, q, q.EnqueueReplace(8, "", "https://danbooru.donmai.us/posts/502"))

			if len(job.Items) != 1 || job.Items[0].Outcome != outcome.Failed || job.Items[0].ErrorCode != tc.want {
				t.Fatalf("items = %+v, want one failed %s", job.Items, tc.want)
			}
			reported := slices.ContainsFunc(cap.paths, func(p string) bool { return strings.Contains(p, "/fetch-status") })
			if reported != tc.reported {
				t.Errorf("paths = %v, want a fetch-status report: %v", cap.paths, tc.reported)
			}
		})
	}
}

func TestPipelineReplaceRefusesMultiPostURL(t *testing.T) {
	fake := &fakeRunner{resolved: []gdl.Item{danbooruPost("1"), danbooruPost("2")}, writeIdx: []int{0, 1}}
	cap := &replaceCapture{}
	q := testEnv(t, fake, replaceStub(t, cap, http.StatusOK, map[string]any{"replaced": true}))

	job := waitJob(t, q, q.EnqueueReplace(9, "", "https://danbooru.donmai.us/posts?tags=cat"))

	if len(job.Items) != 1 || job.Items[0].ErrorCode != outcome.ErrCodeUnsupportedURL {
		t.Fatalf("items = %+v, want one failed unsupported_url", job.Items)
	}
	for _, p := range cap.paths {
		if strings.Contains(p, "/file") {
			t.Errorf("a multi-post url was pushed: %v", cap.paths)
		}
	}
}

func TestPipelineReplaceUnusableFileFailsWithItsReason(t *testing.T) {
	fake := &fakeRunner{resolved: []gdl.Item{danbooruPost("502")}, errIdx: []int{0}}
	cap := &replaceCapture{}
	q := testEnv(t, fake, replaceStub(t, cap, http.StatusOK, map[string]any{"replaced": true}))

	job := waitJob(t, q, q.EnqueueReplace(7, "", "https://danbooru.donmai.us/posts/502"))

	if len(job.Items) != 1 || job.Items[0].ErrorCode != outcome.ErrCodeMappingFailed ||
		!strings.Contains(job.Items[0].Error, "no readable metadata sidecar") {
		t.Fatalf("items = %+v, want one failed with the entry's reason", job.Items)
	}
	for _, p := range cap.paths {
		if strings.Contains(p, "/file") {
			t.Errorf("an unusable download was pushed: %v", cap.paths)
		}
	}
}
