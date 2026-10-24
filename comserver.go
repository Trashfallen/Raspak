package main

// Пункт «Raspak ›» в новом контекстном меню Windows 11.
//
// Новое меню показывает только обработчики IExplorerCommand из пакетов MSIX.
// Raspak регистрирует для себя пакет без файлов (sparse, см. winmenu.go),
// а обработчик - это сам Raspak.exe как COM-сервер вне процесса: проводник
// запускает «Raspak.exe --com-server», получает объекты через прокси COM
// и вызывает их методы. Объекты сделаны вручную: таблицы методов - функции Go
// через windows.NewCallback, память объектов - CoTaskMemAlloc.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CLSID обработчика; тот же прописан в манифесте пакета.
var menuCLSID = windows.GUID{Data1: 0x6B2F3C1A, Data2: 0x9D4E, Data3: 0x4F7B, Data4: [8]byte{0xA3, 0x5C, 0x1E, 0x8D, 0x72, 0x40, 0xB9, 0x5A}}

var (
	iidIUnknown             = windows.GUID{Data1: 0x00000000, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidIClassFactory        = windows.GUID{Data1: 0x00000001, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidIExplorerCommand     = windows.GUID{Data1: 0xA08CE4D0, Data2: 0xFA25, Data3: 0x44AB, Data4: [8]byte{0xB5, 0x7C, 0xC7, 0xB1, 0xC3, 0x23, 0xE0, 0xB9}}
	iidIEnumExplorerCommand = windows.GUID{Data1: 0xA88826F8, Data2: 0x186F, Data3: 0x4987, Data4: [8]byte{0xAA, 0xDE, 0xEA, 0x0C, 0xEF, 0x8F, 0xBF, 0xE8}}
)

const (
	sOK                 = 0
	sFalse              = 1
	eNotImpl            = 0x80004001
	eNoInterface        = 0x80004002
	ePointer            = 0x80004003
	eFail               = 0x80004005
	classENoAggregation = 0x80040110

	ecfHasSubcommands = 0x1
	ecsEnabled        = 0x0
)

// Когда пункт виден.
const (
	forArchives = iota // выделены только архивы
	forFiles           // выделено что-то кроме архивов
	forAll
)

// Пункты подменю. Пакет регистрирует меню для всех файлов и папок, а какие
// пункты показать, решает GetState по выделенному.
var menuItems = []struct {
	title string
	flag  string // аргумент Raspak.exe; пусто - открыть в окне
	show  int
}{
	{"Распаковать здесь", "--extract", forArchives},
	{"Распаковать в…", "--extract-to", forArchives},
	{"Открыть в Raspak", "", forArchives},
	{"Проверить архив", "--test", forArchives},
	{"Добавить в ZIP", "--add", forFiles}, // с выделением - «Добавить в «Фото.zip»»
	{"Добавить в архив…", "--add-dialog", forAll},
}

const ecsHidden = 0x2

func menuTitle(idx int, paths []string) string {
	if menuItems[idx].flag == "--add" && len(paths) > 0 {
		return "Добавить в «" + filepath.Base(packName(paths, ".zip")) + "»"
	}
	return menuItems[idx].title
}

func menuVisible(idx int, paths []string) bool {
	switch menuItems[idx].show {
	case forArchives:
		return allArchives(paths)
	case forFiles:
		return len(paths) > 0 && !allArchives(paths)
	}
	return true
}

const (
	kindFactory = iota
	kindCommand
	kindEnum
)

// comObj лежит в памяти CoTaskMemAlloc: первым полем - таблица методов.
type comObj struct {
	vtbl *uintptr
	refs int32
	kind int32
	idx  int32 // команда: -1 - корень «Raspak», иначе номер пункта; перечислитель: позиция
	_    int32
}

var (
	modOle32ex          = windows.NewLazySystemDLL("ole32.dll")
	procCoTaskMemAlloc  = modOle32ex.NewProc("CoTaskMemAlloc")
	procCoRegisterClass = modOle32ex.NewProc("CoRegisterClassObject")
	procCoRevokeClass   = modOle32ex.NewProc("CoRevokeClassObject")
	procAllowSetFG      = windows.NewLazySystemDLL("user32.dll").NewProc("AllowSetForegroundWindow")

	liveObjects  atomic.Int32 // команды и перечислители
	lastActivity atomic.Int64

	vtblFactory, vtblCommand, vtblEnum []uintptr
	vtblOnce                           sync.Once
)

func touch() { lastActivity.Store(time.Now().UnixNano()) }

func newComObj(kind int32, idx int32) *comObj {
	p, _, _ := procCoTaskMemAlloc.Call(unsafe.Sizeof(comObj{}))
	if p == 0 {
		return nil
	}
	o := (*comObj)(unsafe.Add(unsafe.Pointer(nil), p))
	*o = comObj{refs: 1, kind: kind, idx: idx}
	switch kind {
	case kindFactory:
		o.vtbl = &vtblFactory[0]
	case kindCommand:
		o.vtbl = &vtblCommand[0]
		liveObjects.Add(1)
	case kindEnum:
		o.vtbl = &vtblEnum[0]
		liveObjects.Add(1)
	}
	return o
}

func comString(s string) uintptr {
	u := windows.StringToUTF16(s)
	p, _, _ := procCoTaskMemAlloc.Call(uintptr(len(u) * 2))
	if p == 0 {
		return 0
	}
	copy(unsafe.Slice((*uint16)(unsafe.Add(unsafe.Pointer(nil), p)), len(u)), u)
	return p
}

func initVtables() {
	qi := windows.NewCallback(func(o *comObj, riid *windows.GUID, ppv *unsafe.Pointer) uintptr {
		touch()
		if ppv == nil {
			return ePointer
		}
		ok := *riid == iidIUnknown
		switch o.kind {
		case kindFactory:
			ok = ok || *riid == iidIClassFactory
		case kindCommand:
			ok = ok || *riid == iidIExplorerCommand
		case kindEnum:
			ok = ok || *riid == iidIEnumExplorerCommand
		}
		if !ok {
			*ppv = nil
			return eNoInterface
		}
		atomic.AddInt32(&o.refs, 1)
		*ppv = unsafe.Pointer(o)
		return sOK
	})
	addRef := windows.NewCallback(func(o *comObj) uintptr {
		return uintptr(atomic.AddInt32(&o.refs, 1))
	})
	release := windows.NewCallback(func(o *comObj) uintptr {
		n := atomic.AddInt32(&o.refs, -1)
		if n == 0 && o.kind != kindFactory {
			liveObjects.Add(-1)
			touch()
			windows.CoTaskMemFree(unsafe.Pointer(o))
		}
		return uintptr(max(n, 0))
	})

	// IClassFactory
	createInstance := windows.NewCallback(func(o *comObj, outer unsafe.Pointer, riid *windows.GUID, ppv *unsafe.Pointer) uintptr {
		touch()
		if ppv == nil {
			return ePointer
		}
		*ppv = nil
		if outer != nil {
			return classENoAggregation
		}
		root := newComObj(kindCommand, -1)
		if root == nil {
			return eFail
		}
		hr := comCall(unsafe.Pointer(root), 0, uintptr(unsafe.Pointer(riid)), uintptr(unsafe.Pointer(ppv)))
		comCall(unsafe.Pointer(root), vRelease)
		return hr
	})
	lockServer := windows.NewCallback(func(o *comObj, lock int32) uintptr { return sOK })
	vtblFactory = []uintptr{qi, addRef, release, createInstance, lockServer}

	// IExplorerCommand
	getTitle := windows.NewCallback(func(o *comObj, items unsafe.Pointer, out *uintptr) uintptr {
		touch()
		title := appName
		if o.idx >= 0 {
			var paths []string
			if items != nil {
				paths = shellItemPaths(items)
			}
			title = menuTitle(int(o.idx), paths)
		}
		*out = comString(title)
		return sOK
	})
	getIcon := windows.NewCallback(func(o *comObj, items unsafe.Pointer, out *uintptr) uintptr {
		if o.idx >= 0 {
			*out = 0
			return eNotImpl
		}
		*out = comString(exePath() + ",0")
		return sOK
	})
	getToolTip := windows.NewCallback(func(o *comObj, items unsafe.Pointer, out *uintptr) uintptr {
		*out = 0
		return eNotImpl
	})
	getCanonicalName := windows.NewCallback(func(o *comObj, out *windows.GUID) uintptr {
		g := menuCLSID
		g.Data4[7] += byte(o.idx + 1)
		*out = g
		return sOK
	})
	getState := windows.NewCallback(func(o *comObj, items unsafe.Pointer, okToBeSlow int32, state *uint32) uintptr {
		touch()
		*state = ecsEnabled
		if o.idx >= 0 && items != nil && !menuVisible(int(o.idx), shellItemPaths(items)) {
			*state = ecsHidden
		}
		return sOK
	})
	invoke := windows.NewCallback(func(o *comObj, items unsafe.Pointer, bindCtx unsafe.Pointer) uintptr {
		touch()
		if o.idx < 0 || items == nil {
			return sOK
		}
		runMenuItem(int(o.idx), shellItemPaths(items))
		return sOK
	})
	getFlags := windows.NewCallback(func(o *comObj, flags *uint32) uintptr {
		if o.idx < 0 {
			*flags = ecfHasSubcommands
		} else {
			*flags = 0
		}
		return sOK
	})
	enumSub := windows.NewCallback(func(o *comObj, out *unsafe.Pointer) uintptr {
		touch()
		*out = nil
		if o.idx >= 0 {
			return eNotImpl
		}
		e := newComObj(kindEnum, 0)
		if e == nil {
			return eFail
		}
		*out = unsafe.Pointer(e)
		return sOK
	})
	vtblCommand = []uintptr{qi, addRef, release, getTitle, getIcon, getToolTip, getCanonicalName, getState, invoke, getFlags, enumSub}

	// IEnumExplorerCommand
	next := windows.NewCallback(func(o *comObj, celt uint32, out *unsafe.Pointer, fetched *uint32) uintptr {
		touch()
		arr := unsafe.Slice(out, celt)
		var n uint32
		for n < celt && int(o.idx) < len(menuItems) {
			c := newComObj(kindCommand, o.idx)
			if c == nil {
				break
			}
			arr[n] = unsafe.Pointer(c)
			n++
			o.idx++
		}
		if fetched != nil {
			*fetched = n
		}
		if n == celt {
			return sOK
		}
		return sFalse
	})
	skip := windows.NewCallback(func(o *comObj, celt uint32) uintptr {
		o.idx = int32(min(int(o.idx)+int(celt), len(menuItems)))
		if int(o.idx) >= len(menuItems) {
			return sFalse
		}
		return sOK
	})
	reset := windows.NewCallback(func(o *comObj) uintptr {
		o.idx = 0
		return sOK
	})
	clone := windows.NewCallback(func(o *comObj, out *unsafe.Pointer) uintptr {
		*out = nil
		return eNotImpl
	})
	vtblEnum = []uintptr{qi, addRef, release, next, skip, reset, clone}
}

// shellItemPaths достает пути файлов из IShellItemArray.
func shellItemPaths(arr unsafe.Pointer) []string {
	const vGetCount, vGetItemAt = 7, 8
	var n uint32
	if hr := comCall(arr, vGetCount, uintptr(unsafe.Pointer(&n))); int32(hr) < 0 {
		return nil
	}
	var out []string
	for i := uint32(0); i < n; i++ {
		var item unsafe.Pointer
		if hr := comCall(arr, vGetItemAt, uintptr(i), uintptr(unsafe.Pointer(&item))); int32(hr) < 0 || item == nil {
			continue
		}
		var name *uint16
		if hr := comCall(item, vGetDisplayName, sigdnFileSysPath, uintptr(unsafe.Pointer(&name))); int32(hr) >= 0 && name != nil {
			out = append(out, windows.UTF16PtrToString(name))
			windows.CoTaskMemFree(unsafe.Pointer(name))
		}
		comCall(item, vRelease)
	}
	return out
}

// runMenuItem запускает обычный Raspak.exe с нужным режимом.
func runMenuItem(idx int, files []string) {
	flag := menuItems[idx].flag
	if !isPackMode(strings.TrimPrefix(flag, "--")) {
		files = uniqueVolumes(files)
	}
	if len(files) == 0 {
		return
	}
	exe := exePath()
	start := func(args ...string) {
		cmd := exec.Command(exe, args...)
		cmd.Dir = filepath.Dir(files[0])
		if cmd.Start() == nil {
			procAllowSetFG.Call(uintptr(cmd.Process.Pid))
			go cmd.Wait()
		}
	}
	if flag == "" {
		for _, f := range files { // каждый архив - в своем окне
			start(f)
		}
		return
	}
	// тысяча выделенных файлов в командную строку не влезет - передаем списком
	if len(strings.Join(files, " ")) > 8000 {
		list := filepath.Join(tempRoot(), fmt.Sprintf("list-%d.txt", time.Now().UnixNano()))
		if os.MkdirAll(tempRoot(), 0o755) == nil && os.WriteFile(list, []byte(strings.Join(files, "\n")), 0o644) == nil {
			start(flag, "@"+list)
			return
		}
	}
	start(append([]string{flag}, files...)...)
}

// runComServer - режим «Raspak.exe --com-server»: обслуживаем проводник и
// выходим, когда объектов нет 15 секунд.
func runComServer() {
	vtblOnce.Do(initVtables)
	touch()
	ready := make(chan uintptr, 1)
	quit := make(chan struct{})
	go func() {
		// свой поток в многопоточной модели COM: вызовы приходят на потоки RPC
		runtime.LockOSThread()
		const coinitMultithreaded = 0x0
		procCoInitializeEx.Call(0, coinitMultithreaded)
		defer procCoUninitialize.Call()
		factory := newComObj(kindFactory, 0)
		const clsctxLocalServer, regclsMultipleUse = 4, 1
		var cookie uint32
		hr, _, _ := procCoRegisterClass.Call(uintptr(unsafe.Pointer(&menuCLSID)), uintptr(unsafe.Pointer(factory)),
			clsctxLocalServer, regclsMultipleUse, uintptr(unsafe.Pointer(&cookie)))
		if int32(hr) < 0 {
			ready <- hr
			return
		}
		ready <- 0
		<-quit
		procCoRevokeClass.Call(uintptr(cookie))
	}()
	if hr := <-ready; hr != 0 {
		os.Exit(1)
	}
	for {
		time.Sleep(time.Second)
		idle := time.Since(time.Unix(0, lastActivity.Load()))
		if liveObjects.Load() <= 0 && idle > 15*time.Second || idle > 10*time.Minute {
			break
		}
	}
	close(quit)
	time.Sleep(100 * time.Millisecond)
	os.Exit(0)
}
