package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestZonePropagates(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", "plain.zip"))
	if err != nil {
		t.Fatal(err)
	}
	arc := filepath.Join(t.TempDir(), "plain.zip")
	zone := []byte("[ZoneTransfer]\r\nZoneId=3\r\n")
	if err := os.WriteFile(arc, src, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(arc+zoneStream, zone, 0o644); err != nil {
		t.Skip("нет потоков NTFS:", err)
	}
	a, err := OpenArchive(context.Background(), arc, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := extractAll(t, a)
	for p := range wantFiles {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)) + zoneStream)
		if err != nil || string(got) != string(zone) {
			t.Errorf("%s: пометка %q, %v", p, got, err)
		}
	}
}

func TestReplaceViaTemp(t *testing.T) {
	a := open(t, "plain.zip", "")
	dir := t.TempDir()
	old := filepath.Join(dir, "root.txt")
	os.WriteFile(old, []byte("mine"), 0o644)
	if _, err := Extract(context.Background(), a, ExtractOptions{Dest: dir, Conflict: ConflictReplace}, &Job{}); err != nil {
		t.Fatal(err)
	}
	checkFiles(t, dir, wantFiles)
	if fileExists(old + partSuffix) {
		t.Errorf("остался временный файл %s", old+partSuffix)
	}
	if _, err := os.Stat(old + zoneStream); err == nil {
		t.Errorf("пометка у файла из локального архива")
	}
}
