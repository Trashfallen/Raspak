package main

// Окно «Добавить в архив»: компактное, в стиле Windows 11, на WebView2
// (HTML в ui/pack.html). Окно живет в потоке окон walk: цикл сообщений
// WebView2 обслуживает и окна walk, а главное окно на это время отключено.

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/jchv/go-webview2"
	"github.com/lxn/walk"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"raspak/internal/appicon"
)

//go:embed ui/pack.html
var packHTML string

var (
	procGetDpiForSystem = windows.NewLazySystemDLL("user32.dll").NewProc("GetDpiForSystem")
	procDwmSetAttr      = windows.NewLazySystemDLL("dwmapi.dll").NewProc("DwmSetWindowAttribute")
)

// systemDark: в Windows выбрана темная тема приложений.
func systemDark() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("AppsUseLightTheme")
	return err == nil && v == 0
}

func setDarkTitle(h uintptr, dark bool) {
	var v int32
	if dark {
		v = 1
	}
	const dwmaUseImmersiveDarkMode = 20
	procDwmSetAttr.Call(h, dwmaUseImmersiveDarkMode, uintptr(unsafe.Pointer(&v)), 4)
}

// packWhat - «Фото, отчет.docx и еще 3».
func packWhat(sources []string) string {
	names := make([]string, 0, 2)
	for i, s := range sources {
		if i == 2 {
			break
		}
		names = append(names, filepath.Base(s))
	}
	s := strings.Join(names, ", ")
	if len(sources) > 2 {
		s += " и еще " + plural(len(sources)-2, "объект", "объекта", "объектов")
	}
	return s
}

// packDialog спрашивает имя, формат, сжатие и пароль. ok=false - отмена.
func packDialog(owner walk.Form, sources []string, cfg *Config) (CompressOptions, bool) {
	format, level := cfg.PackFormat, cfg.PackLevel
	if format == "" {
		format = ".zip"
	}
	if level == 0 {
		level = LevelNormal
	}
	if level == LevelUltra && format != ".tar.zst" {
		level = LevelMax
	}
	out := packName(sources, format)

	scale := 1.0
	if d, _, _ := procGetDpiForSystem.Call(); d > 0 {
		scale = float64(d) / 96
	}
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		DataPath:  filepath.Join(dataDir(), "webview"),
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title: "Добавить в архив - " + appName, IconId: 1, Center: true,
			Width: uint(500 * scale), Height: uint(470 * scale),
		},
	})
	if w == nil {
		// нет WebView2 Runtime - без окна, обычный zip рядом
		msg := "Не найден компонент Microsoft Edge WebView2, окно настроек недоступно.\n\nСоздать «" + filepath.Base(packName(sources, ".zip")) + "»?"
		if walk.MsgBox(owner, appName, msg, walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != win.IDYES {
			return CompressOptions{}, false
		}
		return CompressOptions{Out: packName(sources, ".zip"), Format: ".zip", Level: LevelNormal}, true
	}
	h := uintptr(w.Window())
	w.SetSize(int(480*scale), int(420*scale), webview2.HintFixed)
	setDarkTitle(h, systemDark())
	if owner != nil {
		// поверх главного окна, а оно на это время недоступно
		const gwlpHwndParent = -8
		win.SetWindowLongPtr(win.HWND(h), gwlpHwndParent, uintptr(owner.Handle()))
		win.EnableWindow(owner.Handle(), false)
		defer func() {
			win.EnableWindow(owner.Handle(), true)
			win.SetForegroundWindow(owner.Handle())
		}()
	}

	var (
		result CompressOptions
		ok     bool
	)
	closeWin := func() { win.PostMessage(win.HWND(h), win.WM_CLOSE, 0, 0) }
	_ = w.Bind("raspakSubmit", func(r struct {
		Dir, Name, Format, Password string
		Level                       int
	}) {
		result = CompressOptions{Out: filepath.Join(r.Dir, winSafeName(r.Name)), Format: r.Format, Level: r.Level, Password: r.Password}
		ok = true
		cfg.PackFormat, cfg.PackLevel = r.Format, r.Level
		closeWin()
	})
	_ = w.Bind("raspakCancel", func() { closeWin() })
	_ = w.Bind("raspakExists", func(dir, name string) bool { return fileExists(filepath.Join(dir, winSafeName(name))) })
	_ = w.Bind("raspakPickFolder", func(dir string) {
		// не из обработчика события WebView2: диалог крутит свой цикл сообщений
		w.Dispatch(func() {
			p, _ := pickFolderHwnd(h, "Куда положить архив", dir)
			b, _ := json.Marshal(p)
			w.Eval("onFolder(" + string(b) + ")")
		})
	})

	init := map[string]any{
		"icon":    "data:image/png;base64," + base64.StdEncoding.EncodeToString(appicon.PNG(72)),
		"what":    packWhat(sources),
		"dir":     filepath.Dir(out),
		"name":    filepath.Base(out),
		"format":  format,
		"level":   level,
		"formats": [][2]string{{".zip", "ZIP"}, {".tar.gz", "TAR.GZ"}, {".tar.zst", "TAR.ZST"}},
		"levels":  [][2]any{{LevelFast, "Быстро"}, {LevelNormal, "Обычно"}, {LevelMax, "Максимум"}, {LevelUltra, "Ультра"}},
		"ultra":   LevelUltra,
		"max":     LevelMax,
	}
	b, _ := json.Marshal(init)
	w.SetHtml(strings.Replace(packHTML, "<script>", "<script>window.INIT="+string(b)+";", 1))

	// примерный размер архива считается в фоне, пока окно открыто
	ctx, cancel := context.WithCancel(context.Background())
	var estMu sync.Mutex
	go func() {
		est, err := estimatePack(ctx, sources)
		if err != nil {
			return
		}
		b, _ := json.Marshal(est)
		estMu.Lock()
		defer estMu.Unlock()
		if ctx.Err() == nil {
			w.Dispatch(func() { w.Eval("onEstimate(" + string(b) + ")") })
		}
	}()
	w.Run()
	estMu.Lock()
	cancel()
	estMu.Unlock()
	if ok {
		_ = cfg.Save()
	}
	return result, ok
}

// runPack создает архив с окном прогресса.
func runPack(owner walk.Form, sources []string, opt CompressOptions) error {
	return runTask(owner, "Упаковка «"+filepath.Base(opt.Out)+"»", 300*time.Millisecond, func(ctx context.Context, job *Job, _ *Task) error {
		_, err := Compress(ctx, sources, opt, job)
		return err
	})
}
