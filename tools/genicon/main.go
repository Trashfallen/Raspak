// genicon рисует assets/icon.ico и assets/icon-256.png:
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
	if err := os.WriteFile("assets/icon.ico", appicon.ICO(16, 20, 24, 32, 40, 48, 64, 128, 256), 0o644); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("assets/icon-256.png", appicon.PNG(256), 0o644); err != nil {
		log.Fatal(err)
	}
}
