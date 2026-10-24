package main

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestBenchExtract: RASPAK_BENCH=архив RASPAK_BENCH_OUT=папка - замер распаковки.
func TestBenchExtract(t *testing.T) {
	path := os.Getenv("RASPAK_BENCH")
	if path == "" {
		t.Skip("RASPAK_BENCH не задан")
	}
	start := time.Now()
	a, err := OpenArchive(context.Background(), path, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("список: %v, файлов %d", time.Since(start), len(a.Entries))
	start = time.Now()
	res, err := Extract(context.Background(), a, ExtractOptions{Dest: os.Getenv("RASPAK_BENCH_OUT"), Conflict: ConflictReplace}, &Job{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("распаковка: %v, %d файлов, %s, блоков LZMA2: %d", time.Since(start), res.Files, formatSize(res.Bytes), lzma2Segments.Load())
}
