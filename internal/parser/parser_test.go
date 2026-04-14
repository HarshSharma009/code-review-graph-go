package parser

import (
	"context"
	"testing"
)

func TestDetectLanguage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path string
		want string
	}{
		{"main.go", "go"},
		{"app.py", "python"},
		{"index.ts", "typescript"},
		{"component.tsx", "tsx"},
		{"script.js", "javascript"},
		{"lib.rs", "rust"},
		{"Main.java", "java"},
		{"file.c", "c"},
		{"file.cpp", "cpp"},
		{"file.cs", "csharp"},
		{"file.rb", "ruby"},
		{"file.kt", "kotlin"},
		{"file.swift", "swift"},
		{"file.php", "php"},
		{"file.scala", "scala"},
		{"file.lua", "lua"},
		{"script.sh", "bash"},
		{"unknown.xyz", ""},
		{"README.md", ""},
	}

	for _, tc := range tests {
		got := DetectLanguage(tc.path)
		if got != tc.want {
			t.Errorf("DetectLanguage(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestFileHash(t *testing.T) {
	t.Parallel()

	data := []byte("hello world")
	hash := FileHash(data)
	if len(hash) != 64 {
		t.Errorf("expected 64 char hex hash, got %d chars", len(hash))
	}

	hash2 := FileHash(data)
	if hash != hash2 {
		t.Error("same data should produce same hash")
	}

	hash3 := FileHash([]byte("different"))
	if hash == hash3 {
		t.Error("different data should produce different hash")
	}
}

func TestParsePython(t *testing.T) {
	t.Parallel()

	source := []byte(`
class MyClass:
    def method(self, x):
        return x * 2

def standalone():
    pass
`)
	parser := NewCodeParser()
	defer parser.Close()

	ctx := context.Background()
	nodes, edges, err := parser.ParseBytes(ctx, "test.py", source)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	if len(nodes) == 0 {
		t.Fatal("expected at least 1 node")
	}

	// Should have File node
	var hasFile, hasClass, hasFunc bool
	for _, n := range nodes {
		switch n.Kind {
		case "File":
			hasFile = true
		case "Class":
			hasClass = true
		case "Function":
			hasFunc = true
		}
	}

	if !hasFile {
		t.Error("missing File node")
	}
	if !hasClass {
		t.Error("missing Class node for MyClass")
	}
	if !hasFunc {
		t.Error("missing Function node")
	}

	if len(edges) == 0 {
		t.Error("expected at least 1 edge (CONTAINS)")
	}
}

func TestParseGo(t *testing.T) {
	t.Parallel()

	source := []byte(`package main

import "fmt"

type Server struct {
	Port int
}

func (s *Server) Start() {
	fmt.Println("starting")
}

func main() {
	s := &Server{Port: 8080}
	s.Start()
}
`)
	parser := NewCodeParser()
	defer parser.Close()

	ctx := context.Background()
	nodes, edges, err := parser.ParseBytes(ctx, "main.go", source)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	var nodeNames []string
	for _, n := range nodes {
		nodeNames = append(nodeNames, n.Name)
	}

	// Should find Server type and main/Start functions
	found := map[string]bool{"main.go": false, "Server": false, "main": false, "Start": false}
	for _, n := range nodes {
		if _, ok := found[n.Name]; ok {
			found[n.Name] = true
		}
	}

	for name, f := range found {
		if !f {
			t.Errorf("missing node: %s", name)
		}
	}

	if len(edges) == 0 {
		t.Error("expected edges (CONTAINS, IMPORTS_FROM, CALLS)")
	}
}

func TestParseJavaScript(t *testing.T) {
	t.Parallel()

	source := []byte(`
import { fetchData } from './api';

class UserService {
    async getUser(id) {
        return fetchData('/users/' + id);
    }
}

function formatUser(user) {
    return user.name;
}
`)
	parser := NewCodeParser()
	defer parser.Close()

	ctx := context.Background()
	nodes, edges, err := parser.ParseBytes(ctx, "user.js", source)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	if len(nodes) < 3 {
		t.Errorf("expected at least 3 nodes (File, Class, Functions), got %d", len(nodes))
	}

	if len(edges) == 0 {
		t.Error("expected at least 1 edge")
	}
}

func TestParseUnsupportedLanguage(t *testing.T) {
	t.Parallel()

	parser := NewCodeParser()
	defer parser.Close()

	ctx := context.Background()
	_, _, err := parser.ParseBytes(ctx, "file.xyz", []byte("content"))
	if err == nil {
		t.Error("expected error for unsupported file type")
	}
}

func TestIsTestFunction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		lang     string
		filePath string
		want     bool
	}{
		{"TestMain", "go", "main_test.go", true},
		{"main", "go", "main.go", false},
		{"test_something", "python", "test_app.py", true},
		{"helper", "python", "app.py", false},
		{"test", "javascript", "app.test.js", true},
	}

	for _, tc := range tests {
		got := isTestFunction(tc.name, tc.lang, tc.filePath)
		if got != tc.want {
			t.Errorf("isTestFunction(%q, %q, %q) = %v, want %v", tc.name, tc.lang, tc.filePath, got, tc.want)
		}
	}
}
