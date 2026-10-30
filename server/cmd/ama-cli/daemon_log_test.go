package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingLogger_RotatesAtLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")

	// maxBytes=10: writing two lines (each <10 but together over the limit) should trigger a rotate.
	r, err := newRotatingLogger(path, 10, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	// The second line goes past 10 bytes, so it rotates first (hello goes into .1) and then writes into the new file.
	if _, err := r.Write([]byte("world-again")); err != nil {
		t.Fatal(err)
	}

	cur, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(cur) != "world-again" {
		t.Fatalf("当前文件 = %q，期望 world-again", cur)
	}
	bak, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("备份文件不存在: %v", err)
	}
	if strings.TrimSpace(string(bak)) != "hello" {
		t.Fatalf("备份 = %q，期望 hello", bak)
	}
}

func TestRotatingLogger_KeepsOnlyNBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	r, err := newRotatingLogger(path, 5, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Trigger rotate several times in a row; with keep=1 only .1 may exist, never .2.
	for i := 0; i < 5; i++ {
		if _, err := r.Write([]byte("abcdefgh")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path + ".2"); !os.IsNotExist(err) {
		t.Fatal(".2 不应存在（keep=1）")
	}
}
