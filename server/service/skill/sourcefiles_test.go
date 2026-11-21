package skill

import (
	"strings"
	"testing"
)

func TestPackageSourceFilesUsesCommonValidationAndContentChecksum(t *testing.T) {
	files := []SourceFile{
		{Path: "scripts/example.sh", Content: []byte("#!/bin/sh\necho example\n")},
		{Path: "SKILL.md", Content: []byte("---\nname: example\ndescription: Example skill\n---\n\nUse this skill.\n")},
	}
	pkg, err := PackageSourceFiles("example", files)
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Name != "example" || len(pkg.Files) != 1 {
		t.Fatalf("unexpected package: %#v", pkg)
	}
	checksum := SourceContentChecksum(pkg)
	if !strings.HasPrefix(checksum, "sha256:") {
		t.Fatalf("checksum = %q", checksum)
	}
	// Repacking the same files is allowed to produce different zip metadata, but the
	// source-content checksum must remain stable and independent of file input order.
	repacked, err := PackageSourceFiles("example", []SourceFile{files[1], files[0]})
	if err != nil {
		t.Fatal(err)
	}
	if SourceContentChecksum(repacked) != checksum {
		t.Fatal("content checksum depends on input order or zip metadata")
	}
}

func TestPackageSourceFilesRejectsTruncatedInput(t *testing.T) {
	_, err := PackageSourceFiles("example", []SourceFile{{Path: "SKILL.md", Content: []byte("partial"), Truncated: true}})
	if err == nil || !strings.Contains(err.Error(), "截断") {
		t.Fatalf("truncated input error = %v", err)
	}
}

func TestShortSourceVersion(t *testing.T) {
	if got := ShortSourceVersion(""); got != "1" {
		t.Fatalf("empty display version = %q", got)
	}
	if got := ShortSourceVersion("1234567890abcdef"); got != "12345678" {
		t.Fatalf("short display version = %q", got)
	}
}
