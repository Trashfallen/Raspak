// Подпись релиза: рядом с каждым файлом кладет <файл>.sig.
//
//	go run ./tools/signrelease dist/App.exe
//
// Ключ (base64) берется из переменной RELEASE_SIGNING_KEY (в CI - секрет
// репозитория), иначе из файла %USERPROFILE%\release-signing\release.key.
// Без ключа или с ключом не от relsign.PublicKey завершается с ошибкой:
// релиз без верной подписи у пользователей не установится.
package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"raspak/internal/relsign"
)

func main() {
	if len(os.Args) < 2 {
		fail(errors.New("укажите файлы для подписи"))
	}
	seed, err := key()
	if err != nil {
		fail(err)
	}
	for _, p := range os.Args[1:] {
		data, err := os.ReadFile(p)
		if err != nil {
			fail(err)
		}
		asset := filepath.Base(p)
		sig := relsign.Sign(seed, asset, data)
		if relsign.Verify(asset, data, []byte(sig)) != nil {
			fail(errors.New("ключ подписи не подходит к relsign.PublicKey"))
		}
		if err := os.WriteFile(p+".sig", []byte(sig), 0o644); err != nil {
			fail(err)
		}
		fmt.Println("подписан:", p)
	}
}

func key() ([]byte, error) {
	s := os.Getenv("RELEASE_SIGNING_KEY")
	if s == "" {
		home, _ := os.UserHomeDir()
		b, err := os.ReadFile(filepath.Join(home, "release-signing", "release.key"))
		if err != nil {
			return nil, errors.New("нет ключа подписи: RELEASE_SIGNING_KEY пуст и нет файла release-signing\release.key")
		}
		s = string(b)
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(seed) != 32 {
		return nil, errors.New("ключ подписи поврежден")
	}
	return seed, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "signrelease:", err)
	os.Exit(1)
}
