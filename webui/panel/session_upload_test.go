// SPDX-License-Identifier: AGPL-3.0-or-later

package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Xustalis/OpenPanda/internal/config"
	"github.com/Xustalis/OpenPanda/internal/sessions"
)

type testUploadPart struct {
	name string
	data []byte
}

func sessionUploadRequest(t *testing.T, id string, parts ...testUploadPart) *http.Request {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for _, part := range parts {
		file, err := form.CreateFormFile("file", part.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(part.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req := authedReq(http.MethodPost, "/api/sessions/"+id+"/upload", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	return req
}

func TestSessionUploadMentionsResolveFromSession(t *testing.T) {
	for _, workspace := range []string{"worktree", "separate working directory", "node directory"} {
		t.Run(workspace, func(t *testing.T) {
			workPath := t.TempDir()
			var worktrees *sessions.Worktrees
			if workspace == "worktree" {
				if _, err := exec.LookPath("git"); err != nil {
					t.Skip("git not installed")
				}
				workPath = newPanelGitRepo(t)
				var err error
				worktrees, err = sessions.OpenWorktrees(workPath)
				if err != nil {
					t.Fatal(err)
				}
			} else if workspace == "separate working directory" {
				// The composer must be able to round-trip this absolute mention
				// even when a directory component contains spaces.
				workPath = filepath.Join(workPath, "node workspace")
				if err := os.Mkdir(workPath, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cfg := config.Default()
			cfg.Storage.WorkPath = workPath
			store := sessions.NewStore(t.TempDir())
			h := New(Deps{Store: newTestStore(t), Sessions: store, Worktrees: worktrees, Cfg: cfg, StaticDir: t.TempDir(), Token: testToken})
			var id string
			if workspace == "separate working directory" {
				sess, err := store.CreateInDir("attachment", "", t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				id = sess.ID
			} else {
				code, out := doJSON(t, h, jsonReq(http.MethodPost, "/api/sessions", `{"title":"attachment"}`))
				id, _ = out["id"].(string)
				if code != http.StatusOK || id == "" {
					t.Fatalf("create session: %d %v", code, out)
				}
			}
			const content = "attached contents"
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, sessionUploadRequest(t, id, testUploadPart{"notes.txt", []byte(content)}))
			var result struct{ Files []uploadedFile }
			if err := json.Unmarshal(rr.Body.Bytes(), &result); rr.Code != http.StatusOK || err != nil || len(result.Files) != 1 {
				t.Fatalf("upload: %d %s, %v", rr.Code, rr.Body, err)
			}
			file := result.Files[0]
			legacy, err := filepath.Rel(workPath, file.Path)
			if err != nil {
				t.Fatal(err)
			}
			wantMention := filepath.ToSlash(legacy)
			switch workspace {
			case "worktree":
				wantMention = ".openpanda/uploads/" + file.Name
			case "separate working directory":
				wantMention = file.Path
			}
			if filepath.Clean(file.Mention) != filepath.Clean(wantMention) {
				t.Errorf("upload mention = %q, want %q", file.Mention, wantMention)
			}
			for _, ref := range []string{file.Mention, filepath.ToSlash(legacy), file.Path} {
				query := url.Values{"path": {ref}, "session": {id}}
				for _, endpoint := range []string{"read", "file"} {
					code, out := doJSON(t, h, authedReq(http.MethodGet, "/api/fs/"+endpoint+"?"+query.Encode(), nil))
					if code != http.StatusOK || out["content"] != content {
						t.Errorf("%s %q: %d %v", endpoint, ref, code, out)
					}
				}
			}
			var picture bytes.Buffer
			if err := png.Encode(&picture, image.NewNRGBA(image.Rect(0, 0, 2, 2))); err != nil {
				t.Fatal(err)
			}
			rr = httptest.NewRecorder()
			h.ServeHTTP(rr, sessionUploadRequest(t, id, testUploadPart{"preview.png", picture.Bytes()}))
			if err := json.Unmarshal(rr.Body.Bytes(), &result); rr.Code != http.StatusOK || err != nil || len(result.Files) != 1 {
				t.Fatalf("upload image: %d %s, %v", rr.Code, rr.Body, err)
			}
			file = result.Files[0]
			legacy, err = filepath.Rel(workPath, file.Path)
			if err != nil {
				t.Fatal(err)
			}
			for _, ref := range []string{file.Mention, filepath.ToSlash(legacy)} {
				query := url.Values{"path": {ref}, "session": {id}}
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, authedReq(http.MethodGet, "/api/fs/image?"+query.Encode(), nil))
				if rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), picture.Bytes()) {
					t.Errorf("preview %q: %d, %d bytes", ref, rr.Code, rr.Body.Len())
				}
			}
		})
	}
}

func TestSessionUploadFailedBatchLeavesOnlyPreviousFiles(t *testing.T) {
	h, store, work := newSessionDeletePanel(t)
	sess, err := store.Create("upload batch")
	if err != nil {
		t.Fatal(err)
	}
	ok := httptest.NewRecorder()
	h.ServeHTTP(ok, sessionUploadRequest(t, sess.ID, testUploadPart{"existing.png", []byte("kept")}))
	if ok.Code != http.StatusOK {
		t.Fatalf("initial upload: %d %s", ok.Code, ok.Body.String())
	}
	bad := httptest.NewRecorder()
	h.ServeHTTP(bad, sessionUploadRequest(t, sess.ID,
		testUploadPart{"batch-first.png", []byte("would be left behind")},
		testUploadPart{"oversized.png", make([]byte, maxUploadFileBytes+1)}))
	if bad.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload: %d %s", bad.Code, bad.Body.String())
	}
	dir := filepath.Join(work, ".openpanda", "uploads", sess.ID)
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("failed batch left files: %v, err=%v", files, err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, files[0].Name())); err != nil || string(data) != "kept" {
		t.Fatalf("initial upload changed: %q, err=%v", data, err)
	}
}

func TestSessionUploadParallelSameName(t *testing.T) {
	h, store, work := newSessionDeletePanel(t)
	sess, err := store.Create("parallel uploads")
	if err != nil {
		t.Fatal(err)
	}
	const count = 32
	recorders := make([]*httptest.ResponseRecorder, count)
	start := make(chan struct{})
	done := make(chan struct{}, count)
	for i := range recorders {
		recorders[i] = httptest.NewRecorder()
		req := sessionUploadRequest(t, sess.ID, testUploadPart{"same.png", []byte(fmt.Sprint(i))})
		go func() {
			<-start
			h.ServeHTTP(recorders[i], req)
			done <- struct{}{}
		}()
	}
	close(start)
	for range recorders {
		<-done
	}
	seen := make(map[string]bool)
	for i, rr := range recorders {
		var result struct{ Files []uploadedFile }
		if err := json.Unmarshal(rr.Body.Bytes(), &result); rr.Code != http.StatusOK || err != nil || len(result.Files) != 1 {
			t.Errorf("request %d: %d, %s", i, rr.Code, rr.Body.String())
			continue
		}
		path := result.Files[0].Path
		if seen[path] {
			t.Errorf("multiple requests returned the same path: %s", path)
		}
		seen[path] = true
		if data, err := os.ReadFile(path); err != nil || string(data) != fmt.Sprint(i) {
			t.Errorf("request %d stored %q, err=%v", i, data, err)
		}
	}
	dir := filepath.Join(work, ".openpanda", "uploads", sess.ID)
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != count {
		t.Fatalf("upload directory has %d entries, err=%v, want %d", len(entries), err, count)
	}
}

func TestPublishSessionUploadsRollsBackPublishedFilesOnFailure(t *testing.T) {
	dir, staging := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(staging, "0"), []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The second staged file is missing, so publication fails after the
	// first rename and after reserving the second final path.
	files := []uploadedFile{{Name: "same.txt"}, {Name: "same.txt"}}
	h := &handler{}
	if err := h.publishSessionUploads(context.Background(), dir, staging, dir, files); err == nil {
		t.Fatal("publish unexpectedly succeeded")
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("rollback left %v, err=%v", entries, err)
	}
}

func TestPublishSessionUploadsKeepsNewBatchWhilePruning(t *testing.T) {
	dir, staging := t.TempDir(), t.TempDir()
	previous := filepath.Join(dir, "previous.txt")
	if err := os.WriteFile(previous, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(previous, maxUploadTotalBytes); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(staging, "0")
	if err := os.WriteFile(staged, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	earlier := time.Now().Add(-time.Hour)
	if err := os.Chtimes(staged, earlier, earlier); err != nil {
		t.Fatal(err)
	}
	files := []uploadedFile{{Name: "new.txt", Size: 1}}
	h := &handler{}
	if err := h.publishSessionUploads(context.Background(), dir, staging, dir, files); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(files[0].Path); err != nil || string(data) != "a" {
		t.Fatalf("new upload was pruned: %q, err=%v", data, err)
	}
	if _, err := os.Stat(previous); !os.IsNotExist(err) {
		t.Fatalf("expected prior upload to be pruned: %v", err)
	}
}
