package main

import (
	"context"
	"os"
	"testing"
)

// Ручная проверка большого RAR: RASPAK_REPRO=путь go test -run TestReproOpen -v
// (так ловилась ошибка solid-архивов WinRAR 7, см. third_party/rardecode/RASPAK.md).
func TestReproOpen(t *testing.T) {
	p := os.Getenv("RASPAK_REPRO")
	if p == "" {
		t.Skip()
	}
	a, err := OpenArchive(context.Background(), p, "", nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Logf("entries %d", len(a.Entries))
	res, err := Extract(context.Background(), a, ExtractOptions{Test: true}, &Job{})
	t.Logf("test: files %d err %v", res.Files, err)
}
