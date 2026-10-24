package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TestMenuHandlerOutOfProc вызывает зарегистрированный обработчик меню так же,
// как проводник: через другой процесс. Запуск: RASPAK_COMCHECK=1 (нужен
// установленный пакет меню).
func TestMenuHandlerOutOfProc(t *testing.T) {
	if os.Getenv("RASPAK_COMCHECK") == "" {
		t.Skip("RASPAK_COMCHECK не задан")
	}
	procCoInitializeEx.Call(0, coinitApartment)
	var cmd unsafe.Pointer
	hr, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&menuCLSID)), 0, 4, // CLSCTX_LOCAL_SERVER
		uintptr(unsafe.Pointer(&iidIExplorerCommand)), uintptr(unsafe.Pointer(&cmd)))
	if int32(hr) < 0 {
		t.Fatalf("CoCreateInstance: %s", hresultText(hr))
	}
	const vGetTitle, vGetState, vGetFlags, vEnum = 3, 7, 9, 10
	var title *uint16
	hr = comCall(cmd, vGetTitle, 0, uintptr(unsafe.Pointer(&title)))
	t.Logf("GetTitle: %s %q", hresultText(hr), ptrStr(title))
	var state uint32 = 99
	hr = comCall(cmd, vGetState, 0, 1, uintptr(unsafe.Pointer(&state)))
	t.Logf("GetState: %s state=%d", hresultText(hr), state)
	var flags uint32 = 99
	hr = comCall(cmd, vGetFlags, uintptr(unsafe.Pointer(&flags)))
	t.Logf("GetFlags: %s flags=%d", hresultText(hr), flags)
	var enum unsafe.Pointer
	hr = comCall(cmd, vEnum, uintptr(unsafe.Pointer(&enum)))
	t.Logf("EnumSubCommands: %s", hresultText(hr))
	if enum != nil {
		for {
			var sub unsafe.Pointer
			var got uint32
			hr = comCall(enum, 3, 1, uintptr(unsafe.Pointer(&sub)), uintptr(unsafe.Pointer(&got)))
			if got == 0 || sub == nil {
				t.Logf("Next: %s (конец)", hresultText(hr))
				break
			}
			var st *uint16
			comCall(sub, vGetTitle, 0, uintptr(unsafe.Pointer(&st)))
			t.Logf("  пункт %q", ptrStr(st))
			comCall(sub, vRelease)
		}
		comCall(enum, vRelease)
	}
	comCall(cmd, vRelease)
}

func ptrStr(p *uint16) string {
	if p == nil {
		return "<nil>"
	}
	s := windows.UTF16PtrToString(p)
	windows.CoTaskMemFree(unsafe.Pointer(p))
	return s
}

// TestMenuItemsForSelection: что обработчик покажет на папке, файле и архиве -
// тем же путем, что проводник (COM вне процесса, IShellItemArray).
// Запуск: RASPAK_COMCHECK=1.
func TestMenuItemsForSelection(t *testing.T) {
	if os.Getenv("RASPAK_COMCHECK") == "" {
		t.Skip("RASPAK_COMCHECK не задан")
	}
	procCoInitializeEx.Call(0, coinitApartment)
	shell32 := windows.NewLazySystemDLL("shell32.dll")
	mkArray := shell32.NewProc("SHCreateShellItemArrayFromShellItem")
	iidShellItemArray := windows.GUID{Data1: 0xB63EA76D, Data2: 0x1F85, Data3: 0x456F, Data4: [8]byte{0xA1, 0x9C, 0x48, 0x15, 0x9E, 0xFA, 0x85, 0x8B}}
	abs := func(p string) string { a, _ := filepath.Abs(p); return a }
	for _, path := range []string{abs("testdata"), abs("testdata/README.md"), abs("testdata/plain.zip")} {
		var item, arr unsafe.Pointer
		procSHCreateItemFromParsingName.Call(uintptr(unsafe.Pointer(ptr(path))), 0, uintptr(unsafe.Pointer(&iidShellItem)), uintptr(unsafe.Pointer(&item)))
		mkArray.Call(uintptr(item), uintptr(unsafe.Pointer(&iidShellItemArray)), uintptr(unsafe.Pointer(&arr)))
		var cmd unsafe.Pointer
		hr, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&menuCLSID)), 0, 4,
			uintptr(unsafe.Pointer(&iidIExplorerCommand)), uintptr(unsafe.Pointer(&cmd)))
		if int32(hr) < 0 {
			t.Fatalf("CoCreateInstance: %s", hresultText(hr))
		}
		var enum unsafe.Pointer
		comCall(cmd, 10, uintptr(unsafe.Pointer(&enum)))
		var shown []string
		for {
			var sub unsafe.Pointer
			var got uint32
			comCall(enum, 3, 1, uintptr(unsafe.Pointer(&sub)), uintptr(unsafe.Pointer(&got)))
			if got == 0 {
				break
			}
			var state uint32
			comCall(sub, 7, uintptr(arr), 1, uintptr(unsafe.Pointer(&state)))
			var title *uint16
			comCall(sub, 3, uintptr(arr), uintptr(unsafe.Pointer(&title)))
			if state&ecsHidden == 0 {
				shown = append(shown, ptrStr(title))
			} else {
				ptrStr(title)
			}
			comCall(sub, vRelease)
		}
		comCall(enum, vRelease)
		comCall(cmd, vRelease)
		t.Logf("%s:\n      %s", filepath.Base(path), strings.Join(shown, "\n      "))
	}
}

// TestClassicMenu строит классическое контекстное меню (IContextMenu), как
// проводник для «Показать дополнительные параметры», и печатает подменю Raspak.
// Запуск: RASPAK_COMCHECK=1.
func TestClassicMenu(t *testing.T) {
	if os.Getenv("RASPAK_COMCHECK") == "" {
		t.Skip("RASPAK_COMCHECK не задан")
	}
	procCoInitializeEx.Call(0, coinitApartment)
	shell32 := windows.NewLazySystemDLL("shell32.dll")
	user32 := windows.NewLazySystemDLL("user32.dll")
	parse := shell32.NewProc("SHParseDisplayName")
	bindParent := shell32.NewProc("SHBindToParent")
	createMenu := user32.NewProc("CreatePopupMenu")
	count := user32.NewProc("GetMenuItemCount")
	getStr := user32.NewProc("GetMenuStringW")
	getSub := user32.NewProc("GetSubMenu")
	iidShellFolder := windows.GUID{Data1: 0x000214E6, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidContextMenu := windows.GUID{Data1: 0x000214E4, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidContextMenu3 := windows.GUID{Data1: 0xBCFCE0A0, Data2: 0xEC17, Data3: 0x11D0, Data4: [8]byte{0x8D, 0x10, 0x00, 0xA0, 0xC9, 0x0F, 0x27, 0x19}}
	label := func(m uintptr, i int) string {
		buf := make([]uint16, 256)
		getStr.Call(m, uintptr(i), uintptr(unsafe.Pointer(&buf[0])), 256, 0x400) // MF_BYPOSITION
		return strings.ReplaceAll(windows.UTF16ToString(buf), "&", "")
	}
	abs := func(p string) string { a, _ := filepath.Abs(p); return a }
	for _, path := range []string{abs("testdata"), abs("testdata/README.md"), abs("testdata/plain.zip")} {
		var pidl uintptr
		parse.Call(uintptr(unsafe.Pointer(ptr(path))), 0, uintptr(unsafe.Pointer(&pidl)), 0, 0)
		var folder unsafe.Pointer
		var last uintptr
		bindParent.Call(pidl, uintptr(unsafe.Pointer(&iidShellFolder)), uintptr(unsafe.Pointer(&folder)), uintptr(unsafe.Pointer(&last)))
		var cm unsafe.Pointer
		hr := comCall(folder, 10, 0, 1, uintptr(unsafe.Pointer(&last)), uintptr(unsafe.Pointer(&iidContextMenu)), 0, uintptr(unsafe.Pointer(&cm)))
		if int32(hr) < 0 {
			t.Fatalf("GetUIObjectOf: %s", hresultText(hr))
		}
		menu, _, _ := createMenu.Call()
		const cmfExtendedVerbs = 0x100
		comCall(cm, 3, menu, 0, 1, 0x7FFF, cmfExtendedVerbs)
		var cm3 unsafe.Pointer
		comCall(cm, 0, uintptr(unsafe.Pointer(&iidContextMenu3)), uintptr(unsafe.Pointer(&cm3)))
		n, _, _ := count.Call(menu)
		var lines []string
		for i := 0; i < int(n); i++ {
			name := label(menu, i)
			if !strings.Contains(name, "Raspak") {
				continue
			}
			sub, _, _ := getSub.Call(menu, uintptr(i))
			if sub != 0 && cm3 != nil {
				const wmInitMenuPopup = 0x0117
				var res uintptr
				comCall(cm3, 7, wmInitMenuPopup, sub, uintptr(i), uintptr(unsafe.Pointer(&res))) // HandleMenuMsg2: заполнить подменю
			}
			var items []string
			if sub != 0 {
				sn, _, _ := count.Call(sub)
				for j := 0; j < int(sn); j++ {
					items = append(items, label(sub, j))
				}
			}
			lines = append(lines, name+" › "+strings.Join(items, " | "))
		}
		t.Logf("%s: %d пунктов, Raspak: %v", filepath.Base(path), n, lines)
	}
}

// TestApplyClassicMenu переписывает классическое меню и ассоциации так же, как
// программа при запуске (все включено, пакет Windows 11 стоит). RASPAK_APPLYMENU=путь к Raspak.exe.
func TestApplyClassicMenu(t *testing.T) {
	exe := os.Getenv("RASPAK_APPLYMENU")
	if exe == "" {
		t.Skip("RASPAK_APPLYMENU не задан")
	}
	if err := setContextMenu(exe, true, win11Menu()); err != nil {
		t.Fatal(err)
	}
	if err := setAssociations(exe, true); err != nil {
		t.Fatal(err)
	}
}
