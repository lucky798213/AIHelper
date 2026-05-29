package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAndReadFileTools(t *testing.T) {
	baseDir := t.TempDir()

	writer, err := NewWriteFileTool(baseDir)
	if err != nil {
		t.Fatalf("new write tool: %v", err)
	}
	reader, err := NewReadFileTool(baseDir)
	if err != nil {
		t.Fatalf("new read tool: %v", err)
	}

	writeInput, _ := json.Marshal(writeFileInput{
		Path:    "tmp/hello.txt",
		Content: "hello tools",
	})
	result, err := writer.Call(context.Background(), CallContext{}, writeInput)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(result.Content, "wrote 11 bytes") {
		t.Fatalf("write result = %q", result.Content)
	}

	readInput, _ := json.Marshal(readFileInput{Path: "tmp/hello.txt"})
	result, err = reader.Call(context.Background(), CallContext{}, readInput)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if result.Content != "hello tools" {
		t.Fatalf("read content = %q", result.Content)
	}
}

func TestFileToolsRejectPathEscape(t *testing.T) {
	writer, err := NewWriteFileTool(t.TempDir())
	if err != nil {
		t.Fatalf("new write tool: %v", err)
	}

	input, _ := json.Marshal(writeFileInput{
		Path:    "../outside.txt",
		Content: "nope",
	})
	if _, err := writer.Call(context.Background(), CallContext{}, input); err == nil {
		t.Fatal("expected path escape to fail")
	}
}

func TestFileToolsRejectSymlinkTarget(t *testing.T) {
	baseDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(baseDir, "link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	writer, err := NewWriteFileTool(baseDir)
	if err != nil {
		t.Fatalf("new write tool: %v", err)
	}
	input, _ := json.Marshal(writeFileInput{
		Path:    "link.txt",
		Content: "changed",
	})
	if _, err := writer.Call(context.Background(), CallContext{}, input); err == nil {
		t.Fatal("expected symlink write to fail")
	}

	editor, err := NewEditFileTool(baseDir)
	if err != nil {
		t.Fatalf("new edit tool: %v", err)
	}
	editInput, _ := json.Marshal(editFileInput{
		Path:    "link.txt",
		OldText: "outside",
		NewText: "changed",
	})
	if _, err := editor.Call(context.Background(), CallContext{}, editInput); err == nil {
		t.Fatal("expected symlink edit to fail")
	}
}

func TestListFilesTool(t *testing.T) {
	baseDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseDir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.Mkdir(filepath.Join(baseDir, "dir"), 0o755); err != nil {
		t.Fatalf("mkdir dir: %v", err)
	}

	lister, err := NewListFilesTool(baseDir)
	if err != nil {
		t.Fatalf("new list tool: %v", err)
	}
	input, _ := json.Marshal(listFilesInput{Path: "."})
	result, err := lister.Call(context.Background(), CallContext{}, input)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(result.Content, "a.txt") || !strings.Contains(result.Content, "dir/") {
		t.Fatalf("list content = %q", result.Content)
	}
}

func TestEditFileToolReplacesSingleOccurrence(t *testing.T) {
	baseDir := t.TempDir()
	path := filepath.Join(baseDir, "note.txt")
	if err := os.WriteFile(path, []byte("hello old text"), 0o644); err != nil {
		t.Fatalf("write note: %v", err)
	}
	editor, err := NewEditFileTool(baseDir)
	if err != nil {
		t.Fatalf("new edit tool: %v", err)
	}
	input, _ := json.Marshal(editFileInput{
		Path:    "note.txt",
		OldText: "old",
		NewText: "new",
	})
	result, err := editor.Call(context.Background(), CallContext{}, input)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !strings.Contains(result.Content, "replaced 1 occurrence") {
		t.Fatalf("edit result = %q", result.Content)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read note: %v", err)
	}
	if string(raw) != "hello new text" {
		t.Fatalf("edited content = %q", string(raw))
	}
}

func TestEditFileToolRejectsMissingAndAmbiguousText(t *testing.T) {
	baseDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseDir, "note.txt"), []byte("old old"), 0o644); err != nil {
		t.Fatalf("write note: %v", err)
	}
	editor, err := NewEditFileTool(baseDir)
	if err != nil {
		t.Fatalf("new edit tool: %v", err)
	}

	missing, _ := json.Marshal(editFileInput{Path: "note.txt", OldText: "missing", NewText: "new"})
	if _, err := editor.Call(context.Background(), CallContext{}, missing); err == nil {
		t.Fatal("expected missing old_text to fail")
	}

	ambiguous, _ := json.Marshal(editFileInput{Path: "note.txt", OldText: "old", NewText: "new"})
	if _, err := editor.Call(context.Background(), CallContext{}, ambiguous); err == nil {
		t.Fatal("expected ambiguous old_text to fail")
	}
}

func TestEditFileToolReplaceAll(t *testing.T) {
	baseDir := t.TempDir()
	path := filepath.Join(baseDir, "note.txt")
	if err := os.WriteFile(path, []byte("old old"), 0o644); err != nil {
		t.Fatalf("write note: %v", err)
	}
	editor, err := NewEditFileTool(baseDir)
	if err != nil {
		t.Fatalf("new edit tool: %v", err)
	}
	input, _ := json.Marshal(editFileInput{
		Path:       "note.txt",
		OldText:    "old",
		NewText:    "new",
		ReplaceAll: true,
	})
	if _, err := editor.Call(context.Background(), CallContext{}, input); err != nil {
		t.Fatalf("edit: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read note: %v", err)
	}
	if string(raw) != "new new" {
		t.Fatalf("edited content = %q", string(raw))
	}
}
