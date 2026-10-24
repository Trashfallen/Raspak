package main

// Обновление из релизов GitHub: при запуске смотрим последний релиз; если он
// новее, в окне появляется «Обновить до vX». exe качается рядом (.new),
// текущий переименовывается в .old (запущенный exe переименовать можно,
// удалить нельзя), новый встает на место и запускается, старый выходит.
// Новый дожидается выхода старого и удаляет .old.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

const (
	updateRepo  = "Trashfallen/Raspak"
	updateAsset = appName + ".exe"
)

type release struct {
	Tag  string
	URL  string // exe
	Size int64
}

// RASPAK_UPDATE_API - для проверки обновления на локальном сервере.
func updateAPI() string {
	if u := os.Getenv("RASPAK_UPDATE_API"); u != "" {
		return u
	}
	return "https://api.github.com/repos/" + updateRepo + "/releases/latest"
}

// newerVersion: «v1.2.0» новее «1.1.9».
func newerVersion(tag, cur string) bool {
	parse := func(s string) []int {
		s = strings.TrimPrefix(strings.TrimSpace(s), "v")
		s = strings.SplitN(s, "-", 2)[0]
		var out []int
		for _, p := range strings.Split(s, ".") {
			n, err := strconv.Atoi(p)
			if err != nil {
				return nil
			}
			out = append(out, n)
		}
		return out
	}
	a, b := parse(tag), parse(cur)
	if a == nil || b == nil {
		return false
	}
	for i := 0; i < max(len(a), len(b)); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// checkUpdate: nil без ошибки - обновлений нет.
func checkUpdate() (*release, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", updateAPI(), nil)
	req.Header.Set("User-Agent", appName+"/"+appVersion)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub недоступен: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, nil // релизов пока нет
	}
	if resp.StatusCode != 200 {
		return nil, errors.New("GitHub ответил " + resp.Status)
	}
	var rel struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("не разобрал ответ GitHub: %w", err)
	}
	if !newerVersion(rel.Tag, appVersion) {
		return nil, nil
	}
	for _, a := range rel.Assets {
		if strings.EqualFold(a.Name, updateAsset) {
			return &release{Tag: rel.Tag, URL: a.URL, Size: a.Size}, nil
		}
	}
	return nil, nil
}

// downloadUpdate качает новый exe рядом с текущим (.new).
func downloadUpdate(ctx context.Context, rel *release, job *Job) (string, error) {
	if appVersion == "dev" {
		return "", errors.New("обновление работает только в собранной программе")
	}
	path := exePath() + ".new"
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", rel.URL, nil)
	req.Header.Set("User-Agent", appName+"/"+appVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("не удалось скачать: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", errors.New("скачивание: " + resp.Status)
	}
	size := rel.Size
	if resp.ContentLength > 0 {
		size = resp.ContentLength
	}
	job.Total.Store(size)
	job.setCurrent(rel.Tag)
	out, err := os.Create(path)
	if err != nil {
		return "", err
	}
	got, err := copyCtx(ctx, out, resp.Body, make([]byte, 256<<10), job)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return "", fmt.Errorf("обрыв при скачивании: %w", err)
	}
	head := make([]byte, 2)
	if f, err := os.Open(path); err == nil {
		_, _ = io.ReadFull(f, head)
		f.Close()
	}
	if got < 1<<20 || string(head) != "MZ" {
		os.Remove(path)
		return "", errors.New("скачанный файл не похож на программу")
	}
	return path, nil
}

// installAndRestart ставит новый exe на место текущего и запускает его.
func installAndRestart(newExe string, args []string) error {
	exe := exePath()
	oldExe := exe + ".old"
	os.Remove(oldExe)
	if err := os.Rename(exe, oldExe); err != nil {
		os.Remove(newExe)
		return fmt.Errorf("не удалось заменить программу: %w", err)
	}
	if err := os.Rename(newExe, exe); err != nil {
		_ = os.Rename(oldExe, exe)
		return fmt.Errorf("не удалось заменить программу: %w", err)
	}
	cmd := exec.Command(exe, append([]string{"--after-update", strconv.Itoa(os.Getpid())}, args...)...)
	cmd.Dir = filepath.Dir(exe)
	if err := cmd.Start(); err != nil {
		_ = os.Rename(exe, newExe)
		_ = os.Rename(oldExe, exe)
		return fmt.Errorf("новая версия не запустилась: %w", err)
	}
	return nil
}

// waitForExit ждет завершения старой копии после обновления.
func waitForExit(pid int, timeout time.Duration) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	_, _ = windows.WaitForSingleObject(h, uint32(timeout.Milliseconds()))
}

// cleanupOldExe убирает старую копию (.old) и недокачанную (.new).
func cleanupOldExe() {
	exe := exePath()
	for range 20 {
		err := os.Remove(exe + ".old")
		if err == nil || errors.Is(err, os.ErrNotExist) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	_ = os.Remove(exe + ".new")
}
