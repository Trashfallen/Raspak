// Package relsign - подпись релизов. Сборка (CI или release.cmd) подписывает exe
// закрытым ключом из RELEASE_SIGNING_KEY и кладет рядом <exe>.sig. Программа
// ставит обновление, только если подпись сходится с открытым ключом PublicKey:
// подмененный в релизе или по дороге exe не установится.
package relsign

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// PublicKey - открытый ключ подписи релизов. Закрытый хранится вне репозитория.
const PublicKey = "KAVIGd1AalJVSohuAFFMRSdZJ35CmcnduqLMuTGvA5c="

var ErrBadSignature = errors.New("обновление не прошло проверку подписи")

// message: подписывается имя файла и его SHA-256, чтобы exe одной программы
// нельзя было выдать за обновление другой.
func message(asset string, data []byte) []byte {
	h := sha256.Sum256(data)
	return []byte("release-v1|" + strings.ToLower(asset) + "|" + hex.EncodeToString(h[:]))
}

// Sign возвращает содержимое <asset>.sig.
func Sign(seed []byte, asset string, data []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(seed), message(asset, data))) + "\n"
}

// Verify проверяет содержимое <asset>.sig.
func Verify(asset string, data, sig []byte) error { return verifyWith(PublicKey, asset, data, sig) }

func verifyWith(key, asset string, data, sig []byte) error {
	pub, _ := base64.StdEncoding.DecodeString(key)
	s, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || len(s) != ed25519.SignatureSize || len(pub) != ed25519.PublicKeySize ||
		!ed25519.Verify(pub, message(asset, data), s) {
		return ErrBadSignature
	}
	return nil
}

// VerifyFile скачивает подпись по sigURL и проверяет ею файл path.
func VerifyFile(ctx context.Context, sigURL, userAgent, asset, path string) error {
	if sigURL == "" {
		return errors.New("в релизе нет подписи " + asset + ".sig")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", sigURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("не удалось скачать подпись: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("скачивание подписи: %s", resp.Status)
	}
	sig, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return fmt.Errorf("обрыв при скачивании подписи: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return Verify(asset, data, sig)
}
