package main

// Настройки в %APPDATA%\Raspak\config.json (для отладки - RASPAK_HOME).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

type Config struct {
	Asked        bool `json:"integration_asked"` // первый запуск: спросили про проводник
	ContextMenu  bool `json:"context_menu"`
	Associations bool `json:"associations"`
	OpenAfter    bool `json:"open_after"`    // открыть папку после распаковки
	CheckUpdates bool `json:"check_updates"` // смотреть новые версии на GitHub

	Window struct {
		X, Y, W, H int
		Maximized  bool
	} `json:"window"`
	MenuPkg string `json:"menu_package"` // версия и путь exe, с которыми зарегистрировано меню Windows 11

	PackFormat string `json:"pack_format"` // последний выбранный формат нового архива (".zip"...)
	PackLevel  int    `json:"pack_level"`

	mu sync.Mutex
}

func (c *Config) menuPkg() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.MenuPkg
}

func (c *Config) setMenuPkg(s string) {
	c.mu.Lock()
	c.MenuPkg = s
	c.mu.Unlock()
}

func dataDir() string {
	if d := os.Getenv("RASPAK_HOME"); d != "" {
		return d
	}
	base := os.Getenv("APPDATA")
	if base == "" {
		base, _ = os.UserHomeDir()
	}
	return filepath.Join(base, appName)
}

func configPath() string { return filepath.Join(dataDir(), "config.json") }

func loadConfig() *Config {
	c := &Config{OpenAfter: true, CheckUpdates: true}
	if b, err := os.ReadFile(configPath()); err == nil {
		_ = json.Unmarshal(b, c)
	}
	return c
}

func (c *Config) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(dataDir(), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := configPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, configPath())
}
