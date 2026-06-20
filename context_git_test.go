package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetDiffReturnsRawGitDiff(t *testing.T) {
	tmpDir := t.TempDir()

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpDir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test User",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test User",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	run("init", "-b", "main")

	filePath := filepath.Join(tmpDir, "handler.go")
	if err := os.WriteFile(filePath, []byte("package main\n\nfunc HandleHealth() string {\n\treturn \"ok\"\n}\n"), 0644); err != nil {
		t.Fatalf("write baseline file: %v", err)
	}
	run("add", "handler.go")
	run("commit", "-m", "baseline")
	baseSHA := run("rev-parse", "HEAD")

	if err := os.WriteFile(filePath, []byte("package main\n\nfunc HandleHealth() string {\n\treturn \"ok\"\n}\n\nfunc HandleAdminExport() string {\n\treturn \"export\"\n}\n"), 0644); err != nil {
		t.Fatalf("write updated file: %v", err)
	}
	run("add", "handler.go")
	run("commit", "-m", "add export")
	headSHA := run("rev-parse", "HEAD")

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("Chdir(%s) error = %v", tmpDir, err)
	}
	defer func() {
		_ = os.Chdir(origWD)
	}()

	fs := &FilterSet{}
	diff, err := GetDiff(fs, baseSHA, headSHA)
	if err != nil {
		t.Fatalf("GetDiff() error = %v", err)
	}

	if !strings.Contains(diff, "+func HandleAdminExport() string {") {
		t.Fatalf("expected raw diff to contain added function line, got:\n%s", diff)
	}
	if strings.Contains(diff, ":+func HandleAdminExport() string {") {
		t.Fatalf("expected raw diff, but got annotated output:\n%s", diff)
	}
}
