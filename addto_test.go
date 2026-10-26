package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mholt/archives"
)

func TestAddToArchive(t *testing.T) {
	root, files := makeTree(t)
	for _, c := range []struct{ ext, pw string }{{".zip", ""}, {".zip", "секрет"}, {".tar.gz", ""}, {".tar.zst", ""}, {".tar.xz", ""}} {
		t.Run(c.ext+c.pw, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out"+c.ext)
			if c.ext == ".tar.xz" {
				makeTarXz(t, root, out)
			} else if _, err := Compress(context.Background(), []string{root}, CompressOptions{Out: out, Format: c.ext, Password: c.pw}, &Job{}); err != nil {
				t.Fatal(err)
			}
			a, err := OpenArchive(context.Background(), out, c.pw, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := a.canAdd(); err != nil {
				t.Fatal(err)
			}

			// новая папка и замена a.txt - внутрь «Фото отпуск»
			src := t.TempDir()
			os.MkdirAll(filepath.Join(src, "добавка", "sub"), 0o755)
			os.WriteFile(filepath.Join(src, "добавка", "sub", "x.txt"), []byte("новый"), 0o644)
			os.WriteFile(filepath.Join(src, "a.txt"), []byte("заменен"), 0o644)
			items, _, err := collectFiles(context.Background(), []string{filepath.Join(src, "добавка"), filepath.Join(src, "a.txt")}, out)
			if err != nil {
				t.Fatal(err)
			}
			intoDir(items, "Фото отпуск")
			if got := addConflicts(a.Entries, items); len(got) != 1 || got[0] != "Фото отпуск/a.txt" {
				t.Errorf("conflicts %v", got)
			}

			if c.pw != "" {
				before, _ := os.ReadFile(out)
				a.Password = ""
				if err := AddToArchive(context.Background(), a, items, &Job{}); !errors.Is(err, errNeedPassword) {
					t.Errorf("no password: %v", err)
				}
				a.Password = "не тот"
				if err := AddToArchive(context.Background(), a, items, &Job{}); !errors.Is(err, errBadPassword) {
					t.Errorf("wrong password: %v", err)
				}
				if after, _ := os.ReadFile(out); !bytes.Equal(before, after) {
					t.Error("archive changed after failed add")
				}
				a.Password = c.pw
			}
			if err := AddToArchive(context.Background(), a, items, &Job{}); err != nil {
				t.Fatal(err)
			}
			if fileExists(out+".raspak-tmp") || fileExists(out+".raspak-new") {
				t.Error("temp file left")
			}

			a, err = OpenArchive(context.Background(), out, c.pw, nil)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]int{}
			for _, e := range a.Entries {
				seen[e.Path]++
				if c.pw != "" && !e.Dir && !e.Encrypted {
					t.Errorf("%s not encrypted", e.Path)
				}
			}
			if seen["Фото отпуск/a.txt"] != 1 {
				t.Errorf("a.txt entries: %d", seen["Фото отпуск/a.txt"])
			}
			dir := t.TempDir()
			if _, err := Extract(context.Background(), a, ExtractOptions{Dest: dir}, &Job{}); err != nil {
				t.Fatal(err)
			}
			files := maps(files)
			if c.ext == ".tar.xz" {
				delete(files, "big.bin")
			}
			files["a.txt"] = []byte("заменен")
			files["добавка/sub/x.txt"] = []byte("новый")
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

func maps(m map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func makeTarXz(t *testing.T, root, out string) {
	t.Helper()
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	xw, err := archives.Xz{}.OpenWriter(f)
	if err != nil {
		t.Fatal(err)
	}
	items, _, err := collectFiles(context.Background(), []string{root}, "")
	if err != nil {
		t.Fatal(err)
	}
	// big.bin не берем: xz на чистом Go жмет около 1 МБ/с
	items = slices.DeleteFunc(items, func(it packItem) bool { return strings.HasSuffix(it.name, "big.bin") })
	if err := writeTar(context.Background(), xw, items, CompressOptions{Format: ".tar"}, &Job{}); err != nil {
		t.Fatal(err)
	}
	if err := xw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReplacedBy(t *testing.T) {
	drop := replacedBy([]packItem{{name: "a/b.txt"}, {name: "a/d", dir: true}, {name: "f"}})
	for path, want := range map[string]bool{
		"a/B.txt": true,  // тот же файл, регистр не важен
		"a/d":     true,  // запись папки - новая заменит
		"a/d/x":   false, // содержимое папки сливается
		"f/old":   true,  // была папка, стал файл
		"a/c.txt": false,
		"a":       false,
	} {
		if got := drop(path); got != want {
			t.Errorf("%s: %v", path, got)
		}
	}
}

func TestCanAddRejects(t *testing.T) {
	a := &Archive{Path: filepath.Join(tempRoot(), "x.zip"), isZip: true}
	if a.canAdd() == nil {
		t.Error("nested archive accepted")
	}
	a = &Archive{Path: "x.7z", Kind: "7Z", format: archives.SevenZip{}}
	if err := a.canAdd(); err == nil || !strings.Contains(err.Error(), "7Z") {
		t.Errorf("7z: %v", err)
	}
}
