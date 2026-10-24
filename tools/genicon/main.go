// genicon рисует значки: assets/icon.ico (программа), assets/archive.ico
// (файлы-архивы) и assets/icon-256.png для README:
//
//	go run ./tools/genicon
package main

import (
	"log"
	"os"

	"raspak/internal/appicon"
)

func main() {
	if err := os.MkdirAll("assets", 0o755); err != nil {
		log.Fatal(err)
	}
	sizes := []int{16, 20, 24, 32, 40, 48, 64, 128, 256}
	for name, data := range map[string][]byte{
		"assets/icon.ico":     appicon.ICO(sizes...),
		"assets/archive.ico":  appicon.ArchiveICO(sizes...),
		"assets/icon-256.png": appicon.PNG(256),
	} {
		if err := os.WriteFile(name, data, 0o644); err != nil {
			log.Fatal(err)
		}
	}
}
