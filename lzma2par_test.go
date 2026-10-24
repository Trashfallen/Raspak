package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// blocks.7z: 20 МБ текста, сжатого 7-Zip в несколько потоков (-mmt4 -md=256k),
// то есть LZMA2 с многими сбросами словаря (см. testdata/README.md).
const blocksSHA = "0b34873c4b9a63aa2c43bc70acd65e62a7d957359dae90cc7ff4bd935ae09b01"

func extractBlocks(t *testing.T, path string, ctx context.Context) error {
	t.Helper()
	done := make(chan error, 1)
	dir := t.TempDir()
	go func() {
		a, err := OpenArchive(ctx, path, "", nil)
		if err == nil {
			_, err = Extract(ctx, a, ExtractOptions{Dest: dir}, &Job{})
		}
		if err == nil {
			b, rerr := os.ReadFile(filepath.Join(dir, "text.txt"))
			if rerr != nil {
				err = rerr
			} else if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != blocksSHA {
				t.Errorf("содержимое не совпало: %d байт", len(b))
			}
		}
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(60 * time.Second):
		t.Fatal("распаковка зависла")
		return nil
	}
}

func TestParallelLZMA2(t *testing.T) {
	before := lzma2Segments.Load()
	if err := extractBlocks(t, filepath.Join("testdata", "blocks.7z"), context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := lzma2Segments.Load() - before; n < 4 {
		t.Errorf("блоков распаковано параллельно: %d, ждали несколько", n)
	}
}

func TestParallelLZMA2StreamTail(t *testing.T) {
	defer func(v int) { lzma2SegMax = v }(lzma2SegMax)
	lzma2SegMax = 64 << 10 // любой блок «слишком большой» - потоковый режим
	if err := extractBlocks(t, filepath.Join("testdata", "blocks.7z"), context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestParallelLZMA2Corrupt(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "blocks.7z"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 60000; i < 60064; i++ { // испортить середину сжатых данных
		b[i] ^= 0x5A
	}
	p := filepath.Join(t.TempDir(), "bad.7z")
	os.WriteFile(p, b, 0o644)
	if err := extractBlocks(t, p, context.Background()); err == nil {
		t.Fatal("поврежденный архив распаковался без ошибки")
	}
}

func TestParallelLZMA2Cancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := extractBlocks(t, filepath.Join("testdata", "blocks.7z"), ctx); err == nil {
		t.Skip("успело распаковаться до отмены")
	}
}
