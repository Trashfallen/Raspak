package main

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// makeTree: папка с подпапками, русскими именами, пустой папкой и «большим» файлом.
func makeTree(t *testing.T) (string, map[string][]byte) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "Фото отпуск")
	files := map[string][]byte{
		"a.txt":             bytes.Repeat([]byte("hello world\n"), 1000),
		"папка/файл.txt":    []byte("привет\n"),
		"папка/sub/b.jpg":   randBytes(3000),
		"big.bin":           randBytes(packInMemory + 1234), // поток, а не память
		"пустой/":           nil,
		"папка/sub/c.empty": {},
	}
	for name, data := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if strings.HasSuffix(name, "/") {
			os.MkdirAll(p, 0o755)
			continue
		}
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, files
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(int64(n))).Read(b)
	return b
}

func TestCompressRoundTrip(t *testing.T) {
	root, files := makeTree(t)
	for _, c := range []struct {
		ext, pw string
		level   int
	}{
		{".zip", "", 0}, {".zip", "секрет", 0}, {".tar.gz", "", 0}, {".tar.zst", "", 0},
		{".tar.zst", "", LevelUltra}, {".zip", "", LevelUltra}, {".tar.gz", "", LevelUltra}, // у zip и gz - как «Максимум»
	} {
		t.Run(c.ext+c.pw+strconv.Itoa(c.level), func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out"+c.ext)
			res, err := Compress(context.Background(), []string{root}, CompressOptions{Out: out, Format: c.ext, Level: c.level, Password: c.pw}, &Job{})
			if err != nil {
				t.Fatal(err)
			}
			if res.Files != 5 || res.Size == 0 {
				t.Errorf("result %+v", res)
			}
			if fileExists(out + ".raspak-tmp") {
				t.Error("temp file left")
			}
			a, err := OpenArchive(context.Background(), out, c.pw, nil)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if _, err := Extract(context.Background(), a, ExtractOptions{Dest: dir}, &Job{}); err != nil {
				t.Fatal(err)
			}
			for name, data := range files {
				p := filepath.Join(dir, "Фото отпуск", filepath.FromSlash(name))
				if strings.HasSuffix(name, "/") {
					if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
						t.Errorf("dir %s: %v", name, err)
					}
					continue
				}
				got, err := os.ReadFile(p)
				if err != nil || !bytes.Equal(got, data) {
					t.Errorf("%s: err %v, len %d want %d", name, err, len(got), len(data))
				}
			}
		})
	}
}

func TestCompressCancelLeavesNothing(t *testing.T) {
	root, _ := makeTree(t)
	out := filepath.Join(t.TempDir(), "x.zip")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Compress(ctx, []string{root}, CompressOptions{Out: out, Format: ".zip"}, &Job{}); err == nil {
		t.Fatal("want error")
	}
	if fileExists(out) || fileExists(out+".raspak-tmp") {
		t.Error("files left after cancel")
	}
}

func TestPackName(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "Фото"), 0o755)
	os.WriteFile(filepath.Join(dir, "отчет.docx"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), nil, 0o644)
	if got := packName([]string{filepath.Join(dir, "Фото")}, ".zip"); got != filepath.Join(dir, "Фото.zip") {
		t.Errorf("folder: %s", got)
	}
	if got := packName([]string{filepath.Join(dir, "отчет.docx")}, ".zip"); got != filepath.Join(dir, "отчет.zip") {
		t.Errorf("file: %s", got)
	}
	if got := packName([]string{filepath.Join(dir, "отчет.docx"), filepath.Join(dir, "b.txt")}, ".tar.gz"); got != filepath.Join(dir, filepath.Base(dir)+".tar.gz") {
		t.Errorf("many: %s", got)
	}
	os.WriteFile(filepath.Join(dir, "Фото.tar.gz"), nil, 0o644)
	if got := packName([]string{filepath.Join(dir, "Фото")}, ".tar.gz"); got != filepath.Join(dir, "Фото (2).tar.gz") {
		t.Errorf("taken: %s", got)
	}
}

// TestBenchCompress: RASPAK_BENCH_PACK=папка - замер сжатия в zip.
func TestBenchCompress(t *testing.T) {
	src := os.Getenv("RASPAK_BENCH_PACK")
	if src == "" {
		t.Skip("RASPAK_BENCH_PACK не задан")
	}
	for _, ext := range []string{".zip", ".tar.gz", ".tar.zst"} {
		out := filepath.Join(t.TempDir(), "bench"+ext)
		start := time.Now()
		res, err := Compress(context.Background(), []string{src}, CompressOptions{Out: out, Format: ext}, &Job{})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %v, %s", ext, time.Since(start).Round(time.Millisecond), res.sumText())
	}
}

func TestMenuVisibility(t *testing.T) {
	dir := t.TempDir()
	zipf, txt := filepath.Join(dir, "a.zip"), filepath.Join(dir, "b.txt")
	os.WriteFile(zipf, nil, 0o644)
	os.WriteFile(txt, nil, 0o644)
	folder := filepath.Join(dir, "Фото")
	os.Mkdir(folder, 0o755)
	shown := func(paths ...string) string {
		var s []string
		for i := range menuItems {
			if menuVisible(i, paths) {
				s = append(s, menuItems[i].flag)
			}
		}
		return strings.Join(s, " ")
	}
	if got := shown(zipf); got != "--extract --extract-to  --test --add-dialog" {
		t.Errorf("archive: %q", got)
	}
	for _, p := range [][]string{{txt}, {folder}, {zipf, txt}} {
		if got := shown(p...); got != "--add --add-dialog" {
			t.Errorf("%v: %q", p, got)
		}
	}
	if got := menuTitle(4, []string{folder}); got != "Добавить в «Фото.zip»" {
		t.Errorf("title %q", got)
	}
}
