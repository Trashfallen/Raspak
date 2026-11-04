package main

// Перетаскивание из списка архива наружу - в проводник, на рабочий стол,
// в другие программы. Выбранное распаковывается во временную папку, а
// получателю отдается обычный список файлов (SHCreateDataObject), так что
// принимает любая программа, которая понимает перетаскивание файлов.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/lxn/win"
	"golang.org/x/sys/windows"
)

var (
	modShell32drag         = windows.NewLazySystemDLL("shell32.dll")
	procSHDoDragDrop       = modShell32drag.NewProc("SHDoDragDrop")
	procSHCreateDataObject = modShell32drag.NewProc("SHCreateDataObject")
	procILCreateFromPathW  = modShell32drag.NewProc("ILCreateFromPathW")
	procILFindLastID       = modShell32drag.NewProc("ILFindLastID")
	procILFree             = modShell32drag.NewProc("ILFree")
	procGetAsyncKeyState   = windows.NewLazySystemDLL("user32.dll").NewProc("GetAsyncKeyState")

	iidIDataObject = windows.GUID{Data1: 0x0000010e, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
)

const (
	dropEffectCopy = 1
	dragDirPrefix  = "drag-"
)

// tvOrigProc - исходная оконная процедура таблицы (окна-контейнера walk,
// которому список шлет WM_NOTIFY).
var tvOrigProc uintptr

// hookDragOut ловит начало перетаскивания в списке.
func (mw *mainWindow) hookDragOut() {
	proc := windows.NewCallback(func(hwnd win.HWND, msg uint32, wp, lp uintptr) uintptr {
		if msg == win.WM_NOTIFY {
			nmh := (*win.NMHDR)(unsafe.Pointer(lp))
			switch nmh.Code {
			case win.LVN_BEGINDRAG, win.LVN_BEGINRDRAG:
				right := nmh.Code == win.LVN_BEGINRDRAG
				r := win.CallWindowProc(tvOrigProc, hwnd, msg, wp, lp)
				mw.dragOut(right)
				return r
			}
		}
		return win.CallWindowProc(tvOrigProc, hwnd, msg, wp, lp)
	})
	tvOrigProc = win.SetWindowLongPtr(mw.tv.Handle(), win.GWLP_WNDPROC, proc)
}

// dragOut распаковывает выбранное во временную папку и начинает перетаскивание.
func (mw *mainWindow) dragOut(right bool) {
	if mw.arc == nil || mw.dragging {
		return
	}
	var sel []*node
	for _, i := range mw.tv.SelectedIndexes() {
		if i >= 0 && i < len(mw.model.rows) {
			sel = append(sel, mw.model.rows[i])
		}
	}
	if len(sel) == 0 {
		return
	}
	mw.dragging = true
	defer func() { mw.dragging = false }()

	dir := filepath.Join(tempRoot(), fmt.Sprintf("%s%d", dragDirPrefix, time.Now().UnixNano()))
	paths := make([]string, len(sel))
	for i, n := range sel {
		paths[i] = n.path
	}
	title := "Готовлю «" + sel[0].name + "»"
	if len(sel) > 1 {
		title = "Готовлю " + plural(len(sel), "элемент", "элемента", "элементов")
	}
	_, err := extractWithPassword(mw, mw.arc, title, ExtractOptions{
		Dest: dir, Select: paths, Base: mw.cwd, Conflict: ConflictReplace,
	})
	mw.setStatus()
	if err != nil {
		_ = os.RemoveAll(dir)
		if !errors.Is(err, context.Canceled) {
			showError(mw, "Не удалось распаковать для перетаскивания.", err)
		}
		return
	}
	// пока распаковывалось, кнопку могли отпустить - тогда не бросаем
	// файлы туда, где случайно оказался курсор
	if !mouseHeld(right) {
		mw.info.SetText("Перетаскивание отменено: кнопка мыши отпущена")
		return
	}
	var files []string
	for _, n := range sel {
		p := filepath.Join(dir, winSafeName(n.name))
		if _, err := os.Lstat(p); err == nil {
			files = append(files, p)
		}
	}
	if len(files) == 0 {
		return
	}
	mw.lastDragDir = dir
	if err := shellDrag(mw.Handle(), dir, files); err != nil {
		showError(mw, "Не удалось начать перетаскивание.", err)
	}
	// папку не удаляем: проводник копирует уже после отпускания кнопки,
	// в своем потоке; старое уберет cleanTemp
}

// isOwnDrag: в окно бросили то, что сами же тащим из архива.
func (mw *mainWindow) isOwnDrag(files []string) bool {
	if mw.lastDragDir == "" || len(files) == 0 {
		return false
	}
	pref := strings.ToLower(mw.lastDragDir) + `\`
	for _, f := range files {
		if !strings.HasPrefix(strings.ToLower(f), pref) {
			return false
		}
	}
	return true
}

func mouseHeld(right bool) bool {
	vk := win.VK_LBUTTON
	if right {
		vk = win.VK_RBUTTON
	}
	if win.GetSystemMetrics(win.SM_SWAPBUTTON) != 0 {
		if vk == win.VK_LBUTTON {
			vk = win.VK_RBUTTON
		} else {
			vk = win.VK_LBUTTON
		}
	}
	s, _, _ := procGetAsyncKeyState.Call(uintptr(vk))
	return s&0x8000 != 0
}

// shellDrag: объект данных проводника для файлов одной папки и стандартное
// перетаскивание (SHDoDragDrop сам рисует картинку и делает IDropSource).
// Только копирование - чтобы курсор не обещал «переместить» из архива.
func shellDrag(hwnd win.HWND, dir string, files []string) error {
	parent, err := ilFromPath(dir)
	if err != nil {
		return err
	}
	defer procILFree.Call(parent)
	abs := make([]uintptr, 0, len(files))
	defer func() {
		for _, p := range abs {
			procILFree.Call(p)
		}
	}()
	children := make([]uintptr, 0, len(files))
	for _, f := range files {
		p, err := ilFromPath(f)
		if err != nil {
			return err
		}
		abs = append(abs, p)
		last, _, _ := procILFindLastID.Call(p)
		children = append(children, last)
	}
	var obj unsafe.Pointer
	hr, _, _ := procSHCreateDataObject.Call(parent, uintptr(len(children)),
		uintptr(unsafe.Pointer(&children[0])), 0,
		uintptr(unsafe.Pointer(&iidIDataObject)), uintptr(unsafe.Pointer(&obj)))
	if int32(hr) < 0 || obj == nil {
		return fmt.Errorf("SHCreateDataObject: 0x%08X", uint32(hr))
	}
	defer comRelease(obj)
	var effect uint32
	hr, _, _ = procSHDoDragDrop.Call(uintptr(hwnd), uintptr(obj), 0, dropEffectCopy, uintptr(unsafe.Pointer(&effect)))
	if int32(hr) < 0 {
		return fmt.Errorf("SHDoDragDrop: 0x%08X", uint32(hr))
	}
	return nil
}

func ilFromPath(p string) (uintptr, error) {
	s, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return 0, err
	}
	r, _, _ := procILCreateFromPathW.Call(uintptr(unsafe.Pointer(s)))
	if r == 0 {
		return 0, fmt.Errorf("не найден путь %s", p)
	}
	return r, nil
}

// comRelease вызывает IUnknown::Release.
func comRelease(obj unsafe.Pointer) {
	vtbl := *(*[3]uintptr)(*(*unsafe.Pointer)(obj))
	syscall.SyscallN(vtbl[2], uintptr(obj))
}
