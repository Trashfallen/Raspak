package relsign

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func TestVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	data := []byte("MZ program")
	sig := []byte(Sign(priv.Seed(), "App.exe", data))
	// с чужим ключом: подпись не сходится с вшитым PublicKey
	if Verify("App.exe", data, sig) == nil {
		t.Fatal("подпись чужим ключом принята")
	}
	key := base64.StdEncoding.EncodeToString(pub)
	if err := verifyWith(key, "app.EXE", data, sig); err != nil {
		t.Fatalf("верная подпись: %v", err)
	}
	if verifyWith(key, "Other.exe", data, sig) == nil {
		t.Fatal("подпись принята для другого файла")
	}
	if verifyWith(key, "App.exe", append(data, '!'), sig) == nil {
		t.Fatal("подпись принята для измененного файла")
	}
	if verifyWith(key, "App.exe", data, []byte("мусор")) == nil {
		t.Fatal("мусор принят как подпись")
	}
}
