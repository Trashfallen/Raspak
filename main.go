package main

// Raspak - распаковщик архивов для Windows.
//
//	Raspak.exe [архив]                  окно программы
//	Raspak.exe --extract архив...       распаковать рядом (меню проводника)
//	Raspak.exe --extract-to архив...    спросить папку и распаковать туда
//	Raspak.exe --test архив...          проверить целостность
//	Raspak.exe --com-server             обработчик меню Windows 11 (запускает проводник)

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/lxn/win"
	"golang.org/x/sys/windows"
)

const appName = "Raspak"

// appVersion подставляется при сборке (-X main.appVersion=...).
var appVersion = "dev"

var unsafeSizeofPlacement = unsafe.Sizeof(win.WINDOWPLACEMENT{})

func main() {
	var (
		mode        string
		files       []string
		afterUpdate int
	)
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch a := args[i]; strings.ToLower(a) {
		case "--com-server", "-embedding", "/embedding":
			runComServer()
			return
		case "--extract", "--extract-to", "--test":
			mode = strings.ToLower(a[2:])
		case "--after-update":
			if i+1 < len(args) {
				afterUpdate, _ = strconv.Atoi(args[i+1])
				i++
			}
		default:
			files = append(files, a)
		}
	}
	if afterUpdate > 0 {
		waitForExit(afterUpdate, 10*time.Second)
		go cleanupOldExe()
	}
	go cleanTemp()
	loadAppIcon()

	if mode != "" && len(files) > 0 {
		runBatch(mode, files)
		return
	}
	runMain(loadConfig(), files)
}

// tempRoot - папка для файлов, открытых из архива.
func tempRoot() string { return filepath.Join(os.TempDir(), appName) }

// cleanTemp удаляет файлы, открытые из архивов больше суток назад.
func cleanTemp() {
	entries, err := os.ReadDir(tempRoot())
	if err != nil {
		return
	}
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > 24*time.Hour {
			_ = os.RemoveAll(filepath.Join(tempRoot(), e.Name()))
		}
	}
}

func knownDesktop() string {
	if p, err := windows.KnownFolderPath(windows.FOLDERID_Desktop, 0); err == nil {
		return p
	}
	h, _ := os.UserHomeDir()
	return h
}
