package main

import (
	"encoding/hex"
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TestAppxReader открывает пакет системным читателем MSIX - он называет точную
// причину, которую Add-AppxPackage прячет. Запуск: RASPAK_APPX_CHECK=путь.msix
func TestAppxReader(t *testing.T) {
	path := os.Getenv("RASPAK_APPX_CHECK")
	if path == "" {
		t.Skip("RASPAK_APPX_CHECK не задан")
	}
	procCoInitializeEx.Call(0, 0)
	clsid := windows.GUID{Data1: 0x5842a140, Data2: 0xff9f, Data3: 0x4166, Data4: [8]byte{0x8f, 0x5c, 0x62, 0xf5, 0xb7, 0xb0, 0xc7, 0x81}}
	iid := windows.GUID{Data1: 0xbeb94909, Data2: 0xe451, Data3: 0x438b, Data4: [8]byte{0xb5, 0xa7, 0xd7, 0x9e, 0x76, 0x7b, 0x75, 0xd8}}
	var f unsafe.Pointer
	hr, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsid)), 0, 1, uintptr(unsafe.Pointer(&iid)), uintptr(unsafe.Pointer(&f)))
	if int32(hr) < 0 {
		t.Fatalf("AppxFactory: %#x", uint32(hr))
	}
	var stream unsafe.Pointer
	hr, _, _ = windows.NewLazySystemDLL("shlwapi.dll").NewProc("SHCreateStreamOnFileEx").Call(
		uintptr(unsafe.Pointer(ptr(path))), 0, 0x80, 0, 0, uintptr(unsafe.Pointer(&stream)))
	if int32(hr) < 0 {
		t.Fatalf("stream: %#x", uint32(hr))
	}
	var reader unsafe.Pointer
	hr = comCall(f, 4, uintptr(stream), uintptr(unsafe.Pointer(&reader)))
	t.Logf("CreatePackageReader: %#x", uint32(hr))
	if int32(hr) < 0 {
		t.Fail()
	}
}

// TestSignMSIX: RASPAK_SIGN=путь.msix RASPAK_SIGN_THUMB=отпечаток.
func TestSignMSIX(t *testing.T) {
	path, thumb := os.Getenv("RASPAK_SIGN"), os.Getenv("RASPAK_SIGN_THUMB")
	if path == "" {
		t.Skip("RASPAK_SIGN не задан")
	}
	b, err := hex.DecodeString(thumb)
	if err != nil {
		t.Fatal(err)
	}
	if err := signMSIX(path, b); err != nil {
		t.Fatal(err)
	}
}
