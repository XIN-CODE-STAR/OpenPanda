// SPDX-License-Identifier: AGPL-3.0-or-later

package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestListDirectories(t *testing.T) {
	tempDir := t.TempDir()
	sub1 := filepath.Join(tempDir, "alpha")
	sub2 := filepath.Join(tempDir, "beta")
	hidden := filepath.Join(tempDir, ".hidden")
	file := filepath.Join(tempDir, "hello.txt")

	_ = os.MkdirAll(sub1, 0755)
	_ = os.MkdirAll(sub2, 0755)
	_ = os.MkdirAll(hidden, 0755)
	_ = os.WriteFile(file, []byte("test"), 0644)

	h := &handler{}
	req := httptest.NewRequest("GET", "/api/fs/directories?path="+tempDir, nil)
	w := httptest.NewRecorder()

	h.listDirectories(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp listDirectoriesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if resp.Current != tempDir {
		t.Errorf("expected current %q, got %q", tempDir, resp.Current)
	}
	if len(resp.Directories) != 2 {
		t.Errorf("expected 2 directories, got %d", len(resp.Directories))
	}
	names := []string{resp.Directories[0].Name, resp.Directories[1].Name}
	if names[0] != "alpha" || names[1] != "beta" {
		t.Errorf("expected [alpha, beta], got %v", names)
	}
}

// An empty folder must answer `[]`, not `null`: the picker filters the list the
// moment it arrives, and a null there threw before the card could paint — the
// empty directory then read as "does not exist or is unreadable", and the typed
// path could not be created from the search field.
func TestListDirectoriesEmptyIsAnEmptyList(t *testing.T) {
	dir := t.TempDir()
	h := &handler{}
	req := httptest.NewRequest("GET", "/api/fs/directories?path="+dir, nil)
	w := httptest.NewRecorder()

	h.listDirectories(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, `"directories":[]`) {
		t.Fatalf("empty listing answered %s, want \"directories\":[]", body)
	}
}

// stubDirDialog replaces the native chooser for the duration of a test: the
// real one blocks on a modal OS window that no test may raise.
func stubDirDialog(t *testing.T, fn func(initial, title string) (string, bool, error)) {
	t.Helper()
	orig := openDirDialog
	t.Cleanup(func() { openDirDialog = orig })
	openDirDialog = fn
}

func TestChooseDirectoryForwardsDefaults(t *testing.T) {
	var gotInitial, gotTitle string
	stubDirDialog(t, func(initial, title string) (string, bool, error) {
		gotInitial, gotTitle = initial, title
		return `C:/work/sub`, false, nil
	})

	body := strings.NewReader(`{"default_path":"  C:/work/sub  ","title":"选择项目工作目录"}`)
	req := httptest.NewRequest("POST", "/api/dialog/choose-directory", body)
	req.RemoteAddr = "127.0.0.1:5000"
	w := httptest.NewRecorder()

	(&handler{}).chooseDirectoryDialog(w, req)

	if gotInitial != `C:/work/sub` || gotTitle != "选择项目工作目录" {
		t.Fatalf("dialog opened with initial=%q title=%q, want the request's values", gotInitial, gotTitle)
	}
	var resp chooseDirectoryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Path != `C:/work/sub` || resp.Cancelled || resp.Unsupported {
		t.Fatalf("unexpected response %+v", resp)
	}
}

func TestChooseDirectoryCancelIsAnAnswer(t *testing.T) {
	stubDirDialog(t, func(string, string) (string, bool, error) { return "", true, nil })

	req := httptest.NewRequest("POST", "/api/dialog/choose-directory", strings.NewReader(`{}`))
	req.RemoteAddr = "[::1]:5000"
	w := httptest.NewRecorder()
	(&handler{}).chooseDirectoryDialog(w, req)

	var resp chooseDirectoryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Cancelled || resp.Path != "" || resp.Error != "" || resp.Unsupported {
		t.Fatalf("a cancel must be a clean answer, got %+v", resp)
	}
}

func TestChooseDirectoryMarksAMissingDialogUnsupported(t *testing.T) {
	stubDirDialog(t, func(string, string) (string, bool, error) { return "", false, errNoNativeDialog })

	req := httptest.NewRequest("POST", "/api/dialog/choose-directory", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	w := httptest.NewRecorder()
	(&handler{}).chooseDirectoryDialog(w, req)

	var resp chooseDirectoryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Unsupported || resp.Error == "" {
		t.Fatalf("want unsupported with a reason, got %+v", resp)
	}
}

func TestChooseDirectoryRefusesARemoteCaller(t *testing.T) {
	called := false
	stubDirDialog(t, func(string, string) (string, bool, error) {
		called = true
		return `C:/work`, false, nil
	})

	req := httptest.NewRequest("POST", "/api/dialog/choose-directory", strings.NewReader(`{}`))
	req.RemoteAddr = "192.168.1.9:5000"
	w := httptest.NewRecorder()
	(&handler{}).chooseDirectoryDialog(w, req)

	if called {
		t.Fatal("a dialog was raised on the host's desktop for a remote console")
	}
	var resp chooseDirectoryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Unsupported {
		t.Fatalf("want unsupported, got %+v", resp)
	}
}

// A "~/sub" path must expand against the home directory — before the
// resolveFsPath switch only the bare "~" case was handled, so "~/x" fell
// through filepath.Abs as a literal segment and the listing 500'd.
func TestListDirectoriesExpandsTildePrefix(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("~ path expansion is a posix-style convenience")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	sub := filepath.Join(home, "pickme")
	if err := os.MkdirAll(filepath.Join(sub, "inner"), 0755); err != nil {
		t.Fatal(err)
	}

	h := &handler{cfgMu: &sync.RWMutex{}}
	req := httptest.NewRequest("GET", "/api/fs/directories?path=~/pickme", nil)
	w := httptest.NewRecorder()

	h.listDirectories(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}
	var resp listDirectoriesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if resp.Current != sub {
		t.Errorf("expected current %q, got %q", sub, resp.Current)
	}
	if len(resp.Directories) != 1 || resp.Directories[0].Name != "inner" {
		t.Errorf("expected [inner], got %v", resp.Directories)
	}
}
