package main

import (
	"archive/tar"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Архив без записей папок (так часто у репаков): несколько потоков пишут
// первые файлы в одну и ту же еще не созданную папку. Раньше второй поток
// видел папку, созданную первым, и возвращал старую ошибку
// «The system cannot find the path specified».
func TestParallelFilesInNewDirs(t *testing.T) {
	src := filepath.Join(t.TempDir(), "nodirs.tar")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	const dirs, perDir = 40, 25
	for d := range dirs {
		for i := range perDir {
			body := []byte(fmt.Sprintf("%d/%d", d, i))
			hdr := &tar.Header{Name: fmt.Sprintf("game/data/d%02d/sub/f%02d.ogg", d, i), Mode: 0o644, Size: int64(len(body))}
			if err := tw.WriteHeader(hdr); err != nil {
				t.Fatal(err)
			}
			tw.Write(body)
		}
	}
	tw.Close()
	f.Close()

	for round := range 5 {
		a, err := OpenArchive(context.Background(), src, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		dest := t.TempDir()
		res, err := Extract(context.Background(), a, ExtractOptions{Dest: dest, Conflict: ConflictReplace}, &Job{})
		if err != nil {
			t.Fatalf("прогон %d: %v", round, err)
		}
		if res.Files != dirs*perDir {
			t.Fatalf("прогон %d: распаковано %d из %d", round, res.Files, dirs*perDir)
		}
	}
}
