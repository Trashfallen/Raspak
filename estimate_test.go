package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Оценка должна быть близка к настоящему размеру. RASPAK_ESTIMATE_DIR -
// своя папка для проверки на живых данных.
func TestEstimatePack(t *testing.T) {
	root, _ := makeTree(t)
	src := []string{root}
	if d := os.Getenv("RASPAK_ESTIMATE_DIR"); d != "" {
		src = []string{d}
	}
	est, err := estimatePack(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		ext   string
		level int
		pw    string
	}{
		{".zip", LevelFast, ""}, {".zip", LevelNormal, ""}, {".zip", LevelMax, ""}, {".zip", LevelNormal, "x"},
		{".tar.gz", LevelFast, ""}, {".tar.gz", LevelMax, ""},
		{".tar.zst", LevelFast, ""}, {".tar.zst", LevelNormal, ""}, {".tar.zst", LevelMax, ""}, {".tar.zst", LevelUltra, ""},
	} {
		out := filepath.Join(t.TempDir(), "out"+c.ext)
		res, err := Compress(context.Background(), src, CompressOptions{Out: out, Format: c.ext, Level: c.level, Password: c.pw}, &Job{})
		if err != nil {
			t.Fatal(err)
		}
		guess := est.Sizes[c.ext][c.level]
		if c.pw != "" {
			guess = est.ZipPw
		}
		diff := float64(guess-res.Size) / float64(res.Size)
		t.Logf("%s/%d pw=%v: оценка %s, на деле %s (%+.1f%%)", c.ext, c.level, c.pw != "", formatSize(guess), formatSize(res.Size), diff*100)
		if c.level == LevelUltra && diff > 0 {
			continue // для «Ультра» оценка - верхняя граница
		}
		if diff > 0.25 || diff < -0.25 {
			t.Errorf("%s/%d: оценка %d, на деле %d", c.ext, c.level, guess, res.Size)
		}
	}
}
