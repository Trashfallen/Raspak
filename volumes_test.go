package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func touchFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCheckVolumes(t *testing.T) {
	dir := t.TempDir()
	// как в загрузках: первый и последний тома еще качаются
	touchFiles(t, dir, "Game.v1-X.part1.rar.part", "Game.v1-X.part2.rar", "Game.v1-X.part3.rar", "Game.v1-X.part4.rar.part", "other.part1.rar")
	err := checkVolumes(filepath.Join(dir, "Game.v1-X.part2.rar"))
	var ve *volumesError
	if !errors.As(err, &ve) {
		t.Fatalf("want volumesError, got %v", err)
	}
	if strings.Join(ve.missing, ",") != "Game.v1-X.part1.rar,Game.v1-X.part4.rar" {
		t.Errorf("missing %v", ve.missing)
	}
	if len(ve.downloading) != 2 {
		t.Errorf("downloading %v", ve.downloading)
	}

	// все на месте, номера с нулями
	touchFiles(t, dir, "Full.part01.rar", "Full.part02.rar", "Full.part03.rar")
	if err := checkVolumes(filepath.Join(dir, "Full.part01.rar")); err != nil {
		t.Errorf("complete set: %v", err)
	}
	// пропуск в .001
	touchFiles(t, dir, "disk.7z.001", "disk.7z.003")
	if err := checkVolumes(filepath.Join(dir, "disk.7z.001")); err == nil || !strings.Contains(err.Error(), "disk.7z.002") {
		t.Errorf(".001 gap: %v", err)
	}
	// не тома - не проверяем
	if err := checkVolumes(filepath.Join(dir, "plain.zip")); err != nil {
		t.Errorf("plain: %v", err)
	}
}

func TestUniqueVolumes(t *testing.T) {
	in := []string{`D:\dl\G.part3.rar`, `D:\dl\G.part1.rar`, `D:\dl\g.PART2.rar`, `D:\dl\a.zip`, `D:\dl\d.7z.002`, `D:\dl\d.7z.001`, `D:\dl\a.zip`}
	got := uniqueVolumes(in)
	want := []string{`D:\dl\G.part3.rar`, `D:\dl\a.zip`, `D:\dl\d.7z.002`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %v", got)
	}
}

func TestMissingLastVolume(t *testing.T) {
	dir := t.TempDir()
	b, err := os.ReadFile(filepath.Join("testdata", "multi.part01.rar"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "multi.part01.rar"), b, 0o644)
	a, err := OpenArchive(context.Background(), filepath.Join(dir, "multi.part01.rar"), "", nil)
	if err == nil {
		_, err = Extract(context.Background(), a, ExtractOptions{Dest: t.TempDir()}, &Job{})
	}
	if err == nil || !strings.Contains(err.Error(), "multi.part02.rar") {
		t.Fatalf("want missing part02, got %v", err)
	}
}

// TestCheckVolumesEnv: RASPAK_VOLCHECK=путь к тому - что скажет проверка набора.
func TestCheckVolumesEnv(t *testing.T) {
	p := os.Getenv("RASPAK_VOLCHECK")
	if p == "" {
		t.Skip("RASPAK_VOLCHECK не задан")
	}
	t.Logf("первый том: %s", firstVolume(p))
	t.Logf("проверка: %v", checkVolumes(firstVolume(p)))
}
