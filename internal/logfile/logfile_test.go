package logfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "x.log")
	w, err := Open(path, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"aaaaaaaa\n", "bbbbbbbb\n", "cccccccc\n", "dddddddd\n"} {
		if _, err := w.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		return string(b)
	}
	if got := read(path); got != "dddddddd\n" {
		t.Errorf("active = %q", got)
	}
	if got := read(Backup(path, 1)); got != "cccccccc\n" {
		t.Errorf("backup 1 = %q", got)
	}
	if got := read(Backup(path, 2)); got != "bbbbbbbb\n" {
		t.Errorf("backup 2 = %q", got)
	}
	if _, err := os.Stat(Backup(path, 3)); !os.IsNotExist(err) {
		t.Error("only 2 backups should be kept")
	}

	files := Files(path, 2)
	var joined []string
	for _, f := range files {
		joined = append(joined, strings.TrimSpace(read(f)))
	}
	if strings.Join(joined, ",") != "bbbbbbbb,cccccccc,dddddddd" {
		t.Errorf("Files order = %v", joined)
	}
}

func TestAppendsToExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := Open(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("new\n"))
	w.Close()
	if b, _ := os.ReadFile(path); string(b) != "old\nnew\n" {
		t.Errorf("content = %q", b)
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Error("write after close should fail")
	}
}
