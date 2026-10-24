package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildMenuPackage(t *testing.T) {
	out := filepath.Join(t.TempDir(), "menu.msix")
	if p := os.Getenv("RASPAK_MSIX_OUT"); p != "" {
		out = p
	}
	if err := buildMenuPackage(out, "Raspak.exe", envOr("RASPAK_PUBLISHER", menuPkgPublisher)); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(out) // заодно проверка, что zip читается
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if len(names) != 6 || names[0] != "AppxManifest.xml" || names[5] != "[Content_Types].xml" {
		t.Errorf("parts: %v", names)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
