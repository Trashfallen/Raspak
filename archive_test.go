package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Архивы в testdata сделаны 7-Zip из одного набора файлов (см. testdata/README.md).
var wantFiles = map[string]string{
	"folder/a.txt":     "alpha\n",
	"folder/sub/b.txt": "bravo\n",
	"root.txt":         "root file\n",
}

func open(t *testing.T, name, pw string) *Archive {
	t.Helper()
	a, err := OpenArchive(context.Background(), filepath.Join("testdata", name), pw, nil)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return a
}

func extractAll(t *testing.T, a *Archive) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := Extract(context.Background(), a, ExtractOptions{Dest: dir, Conflict: ConflictReplace}, &Job{}); err != nil {
		t.Fatalf("%s: %v", a.Name, err)
	}
	return dir
}

func checkFiles(t *testing.T, dir string, want map[string]string) {
	t.Helper()
	for p, content := range want {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if string(b) != content {
			t.Errorf("%s = %q, want %q", p, b, content)
		}
	}
}

func TestFormats(t *testing.T) {
	for _, name := range []string{"plain.zip", "plain.7z", "plain.tar.gz", "split.7z.001", "split.7z.002"} {
		t.Run(name, func(t *testing.T) {
			a := open(t, name, "")
			dir := extractAll(t, a)
			checkFiles(t, dir, wantFiles)
			if fi, err := os.Stat(filepath.Join(dir, "folder", "rand.bin")); err != nil || fi.Size() != 5000 {
				t.Errorf("rand.bin: %v", err)
			}
		})
	}
}

func TestCyrillicZipNames(t *testing.T) {
	for _, name := range []string{"plain.zip", "plain.7z", "plain.tar.gz"} {
		dir := extractAll(t, open(t, name, ""))
		checkFiles(t, dir, map[string]string{"папка/файл.txt": "привет\n"})
	}
}

func TestSingleCompressed(t *testing.T) {
	a := open(t, "root.txt.gz", "")
	if !a.single || len(a.Entries) != 1 || a.Entries[0].Path != "root.txt" {
		t.Fatalf("entries %+v", a.Entries)
	}
	checkFiles(t, extractAll(t, a), map[string]string{"root.txt": "root file\n"})
}

func TestMultiVolumeRar(t *testing.T) {
	for _, name := range []string{"multi.part01.rar", "multi.part02.rar"} {
		a := open(t, name, "")
		if a.Kind != "RAR" || len(a.Entries) == 0 {
			t.Fatalf("%s: kind %s, %d entries", name, a.Kind, len(a.Entries))
		}
		dir := extractAll(t, a)
		var n int
		filepath.Walk(dir, func(_ string, fi os.FileInfo, _ error) error {
			if fi != nil && !fi.IsDir() && fi.Size() > 0 {
				n++
			}
			return nil
		})
		if n == 0 {
			t.Errorf("%s: nothing extracted", name)
		}
	}
}

func TestPasswords(t *testing.T) {
	for _, name := range []string{"secret.7z", "secret-aes.zip", "secret-zc.zip"} {
		t.Run(name, func(t *testing.T) {
			// без пароля: 7z с шифрованными заголовками не открывается, zip - не распаковывается
			a, err := OpenArchive(context.Background(), filepath.Join("testdata", name), "", nil)
			if err == nil {
				_, err = Extract(context.Background(), a, ExtractOptions{Dest: t.TempDir()}, &Job{})
			}
			if !errors.Is(err, errNeedPassword) {
				t.Fatalf("no password: %v", err)
			}
			a, err = OpenArchive(context.Background(), filepath.Join("testdata", name), "wrong", nil)
			if err == nil {
				dir := t.TempDir()
				_, err = Extract(context.Background(), a, ExtractOptions{Dest: dir}, &Job{})
				if left, _ := os.ReadDir(filepath.Join(dir, "folder")); err != nil && len(left) > 1 {
					t.Errorf("wrong password left files: %v", left)
				}
			}
			if !errors.Is(err, errBadPassword) {
				t.Fatalf("wrong password: %v", err)
			}
			checkFiles(t, extractAll(t, open(t, name, "secret")), wantFiles)
		})
	}
}

func TestTestMode(t *testing.T) {
	a := open(t, "plain.7z", "")
	res, err := Extract(context.Background(), a, ExtractOptions{Test: true}, &Job{})
	if err != nil || res.Files != 5 {
		t.Fatalf("test: %+v %v", res, err)
	}
}

func TestSelectAndBase(t *testing.T) {
	a := open(t, "plain.zip", "")
	dir := t.TempDir()
	_, err := Extract(context.Background(), a, ExtractOptions{Dest: dir, Select: []string{"folder/sub"}, Base: "folder"}, &Job{})
	if err != nil {
		t.Fatal(err)
	}
	checkFiles(t, dir, map[string]string{"sub/b.txt": "bravo\n"})
	if fileExists(filepath.Join(dir, "a.txt")) || fileExists(filepath.Join(dir, "root.txt")) {
		t.Error("extracted more than selected")
	}
}

func TestConflicts(t *testing.T) {
	a := open(t, "plain.zip", "")
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "root.txt"), []byte("mine"), 0o644)
	asked := 0
	res, err := Extract(context.Background(), a, ExtractOptions{Dest: dir, Ask: func(string, Entry, os.FileInfo) (Conflict, bool) {
		asked++
		return ConflictSkip, false
	}}, &Job{})
	if err != nil || asked != 1 || res.Existing != 1 {
		t.Fatalf("asked %d, %+v, %v", asked, res, err)
	}
	checkFiles(t, dir, map[string]string{"root.txt": "mine", "folder/a.txt": "alpha\n"})
}

func TestSafeJoin(t *testing.T) {
	dest := `C:\out`
	for in, want := range map[string]string{
		"a/b.txt":          `C:\out\a\b.txt`,
		"/etc/passwd":      `C:\out\etc\passwd`,
		`C:\Windows\x.dll`: `C:\out\C_\Windows\x.dll`,
		"con.txt":          `C:\out\_con.txt`,
		"a:b?.txt":         `C:\out\a_b_.txt`,
		"dots... ":         `C:\out\dots`,
		"../evil":          "",
		"a/../../evil":     "",
		"..":               "",
		"":                 "",
	} {
		got, ok := safeJoin(dest, in)
		if !ok {
			got = ""
		}
		if got != want {
			t.Errorf("safeJoin(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNames(t *testing.T) {
	for in, want := range map[string]string{
		"фото.zip":        "фото",
		"x.tar.gz":        "x",
		"x.tgz":           "x",
		"x.part01.rar":    "x",
		"x.7z.001":        "x",
		"version.1.2.zip": "version.1.2",
		".zip":            "archive",
	} {
		if got := archiveStem(in); got != want {
			t.Errorf("archiveStem(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"report.txt.gz": "report.txt", "x.tgz": "x.tar", "data.xz": "data", "noext": "noext.out"} {
		if got := singleName(in); got != want {
			t.Errorf("singleName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSmartDest(t *testing.T) {
	base := t.TempDir()
	one := &Archive{Name: "proj.zip", Entries: []Entry{{Path: "proj", Dir: true}, {Path: "proj/a.txt"}}}
	many := &Archive{Name: "pics.zip", Entries: []Entry{{Path: "a.jpg"}, {Path: "b.jpg"}}}

	if d, r := smartDest(one, base); d != base || r != filepath.Join(base, "proj") {
		t.Errorf("one root: %s %s", d, r)
	}
	os.Mkdir(filepath.Join(base, "proj"), 0o755)
	if d, _ := smartDest(one, base); d != filepath.Join(base, "proj (2)") {
		t.Errorf("one root taken: %s", d)
	}
	if d, _ := smartDest(many, base); d != filepath.Join(base, "pics") {
		t.Errorf("many roots: %s", d)
	}
}

func TestFormatSize(t *testing.T) {
	got := []string{formatSize(512), formatSize(1536), formatSize(5 << 30), formatSize(-1)}
	want := []string{"512 Б", "1,5 КБ", "5,0 ГБ", ""}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("%v", got)
	}
	_ = sort.Strings
}
