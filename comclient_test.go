package main

import (
	"os"
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
