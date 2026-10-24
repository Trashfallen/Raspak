package main

// Связь с Windows: меню проводника и ассоциации (все в HKCU, без прав
// администратора), открытие файлов и папок, выбор папки, значки файлов.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	modShell32 = windows.NewLazySystemDLL("shell32.dll")
	modOle32   = windows.NewLazySystemDLL("ole32.dll")

	procSHGetFileInfoW              = modShell32.NewProc("SHGetFileInfoW")
	procSHChangeNotify              = modShell32.NewProc("SHChangeNotify")
	procSHCreateItemFromParsingName = modShell32.NewProc("SHCreateItemFromParsingName")
	procCoInitializeEx              = modOle32.NewProc("CoInitializeEx")
	procCoUninitialize              = modOle32.NewProc("CoUninitialize")
	procCoCreateInstance            = modOle32.NewProc("CoCreateInstance")
	procCoTaskMemFree               = modOle32.NewProc("CoTaskMemFree")
)

// Расширения, для которых Raspak встраивается в проводник.
var archiveExts = []string{
	".zip", ".7z", ".rar", ".tar", ".gz", ".tgz", ".bz2", ".tbz", ".tbz2", ".xz", ".txz",
	".zst", ".tzst", ".lz4", ".br", ".lz", ".sz", ".001",
}

const (
	classesKey = `Software\Classes`
	progID     = appName + ".archive"
	capsKey    = `Software\` + appName + `\Capabilities`
)

func regSet(path, name, value string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(name, value)
}

func regGet(path, name string) string {
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, _ := k.GetStringValue(name)
	return v
}

func regDeleteValue(path, name string) {
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	_ = k.DeleteValue(name)
}

// regDeleteTree удаляет ключ со всеми подключами.
func regDeleteTree(path string) {
	if k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.ENUMERATE_SUB_KEYS); err == nil {
		names, _ := k.ReadSubKeyNames(-1)
		k.Close()
		for _, n := range names {
			regDeleteTree(path + `\` + n)
		}
	}
	_ = registry.DeleteKey(registry.CURRENT_USER, path)
}

func notifyAssocChanged() {
	const shcneAssocChanged = 0x08000000
	procSHChangeNotify.Call(shcneAssocChanged, 0, 0, 0)
}

// writeCascade пишет подменю «Raspak» с пунктами {ключ, название, команда}.
// appliesTo (AQS) ограничивает, для каких файлов оно видно.
func writeCascade(key, exe, appliesTo string, items [][3]string) error {
	q := `"` + exe + `"`
	vals := [][2]string{{"MUIVerb", appName}, {"Icon", q + ",0"}, {"SubCommands", ""}}
	if appliesTo != "" {
		vals = append(vals, [2]string{"AppliesTo", appliesTo})
	}
	for _, v := range vals {
		if err := regSet(key, v[0], v[1]); err != nil {
			return err
		}
	}
	for _, it := range items {
		// Player: без лимита в 15 выделенных файлов; копии программы соберут их в одно задание
		for _, v := range [][3]string{{key + `\shell\` + it[0], "MUIVerb", it[1]}, {key + `\shell\` + it[0], "MultiSelectModel", "Player"},
			{key + `\shell\` + it[0] + `\command`, "", it[2]}} {
			if err := regSet(v[0], v[1], v[2]); err != nil {
				return err
			}
		}
	}
	return nil
}

// setContextMenu: подменю «Raspak» в классическом контекстном меню: на архивах -
// распаковка, на остальных файлах и папках - упаковка. В Windows 11 оно в
// «Показать дополнительные параметры» (Shift+F10).
//
// withPackage: стоит пакет меню Windows 11. Его пункт на файлах и архивах
// проводник показывает и в классическом меню - тогда реестровые подменю для
// файлов и архивов не пишем, иначе «Raspak» будет дважды. На папках пакетный
// пункт в классическое меню не попадает, там подменю из реестра остается.
func setContextMenu(exe string, on, withPackage bool) error {
	q := `"` + exe + `"`
	addDialog := [3]string{"5adddialog", "Добавить в архив…", q + ` --add-dialog "%1"`}
	items := [][3]string{
		{"1open", "Открыть", q + ` "%1"`},
		{"2extract", "Распаковать здесь", q + ` --extract "%1"`},
		{"3extractto", "Распаковать в…", q + ` --extract-to "%1"`},
		{"4test", "Проверить архив", q + ` --test "%1"`},
		addDialog,
	}
	packItems := [][3]string{
		{"1add", "Добавить в ZIP-архив", q + ` --add "%1"`},
		{"2adddialog", "Добавить в архив…", q + ` --add-dialog "%1"`},
	}
	// у архивов свое подменю - упаковочное на них не показываем
	var notArchive []string
	for _, ext := range archiveExts {
		notArchive = append(notArchive, "System.FileExtension:="+ext)
	}
	appliesTo := "NOT (" + strings.Join(notArchive, " OR ") + ")"
	for _, k := range []string{classesKey + `\*\shell\` + appName, classesKey + `\Directory\shell\` + appName} {
		regDeleteTree(k)
		dir := strings.Contains(k, `\Directory\`)
		if !on || withPackage && !dir {
			continue
		}
		at := appliesTo
		if dir {
			at = ""
		}
		if err := writeCascade(k, exe, at, packItems); err != nil {
			return err
		}
	}
	for _, ext := range archiveExts {
		key := classesKey + `\SystemFileAssociations\` + ext + `\shell\` + appName
		// запасное «Открыть»: если программа по умолчанию удалена (UserChoice
		// указывает на несуществующий класс), проводник иначе делает действием
		// по умолчанию подменю «Raspak», и двойной клик ничего не открывает.
		// Живая программа по умолчанию со своим «open» этот пункт перекрывает.
		open := classesKey + `\SystemFileAssociations\` + ext + `\shell\open`
		if strings.Contains(strings.ToLower(regGet(open+`\command`, "")), strings.ToLower(filepath.Base(exe))) {
			regDeleteTree(open)
		}
		regDeleteTree(key)
		if !on {
			continue
		}
		if regGet(open+`\command`, "") == "" {
			if err := regSet(open, "Icon", q+",0"); err != nil {
				return err
			}
			if err := regSet(open+`\command`, "", q+` "%1"`); err != nil {
				return err
			}
		}
		if withPackage {
			continue
		}
		if err := writeCascade(key, exe, "", items); err != nil {
			return err
		}
	}
	notifyAssocChanged()
	return nil
}

// setAssociations: двойной клик по архиву открывает Raspak. Если пользователь
// явно выбрал другую программу (UserChoice), Windows оставит ее - тогда
// поменять можно в «Приложениях по умолчанию».
func setAssociations(exe string, on bool) error {
	prog := classesKey + `\` + progID
	if !on {
		for _, ext := range archiveExts {
			regDeleteValue(classesKey+`\`+ext+`\OpenWithProgids`, progID)
			if regGet(classesKey+`\`+ext, "") == progID {
				regDeleteValue(classesKey+`\`+ext, "")
			}
		}
		regDeleteTree(prog)
		regDeleteTree(`Software\` + appName)
		regDeleteTree(classesKey + `\Applications\` + filepath.Base(exe))
		regDeleteValue(`Software\RegisteredApplications`, appName)
		notifyAssocChanged()
		return nil
	}
	cmd := `"` + exe + `" "%1"`
	steps := [][3]string{
		{prog, "", "Архив"},
		{prog + `\DefaultIcon`, "", `"` + exe + `",0`},
		{prog + `\shell\open\command`, "", cmd},
		{classesKey + `\Applications\` + filepath.Base(exe) + `\shell\open\command`, "", cmd},
		{capsKey, "ApplicationName", appName},
		{capsKey, "ApplicationDescription", "Распаковщик архивов"},
		{`Software\RegisteredApplications`, appName, capsKey},
	}
	for _, ext := range archiveExts {
		steps = append(steps,
			[3]string{classesKey + `\` + ext + `\OpenWithProgids`, progID, ""},
			[3]string{capsKey + `\FileAssociations`, ext, progID})
		if cur := regGet(classesKey+`\`+ext, ""); cur == "" || cur == progID {
			steps = append(steps, [3]string{classesKey + `\` + ext, "", progID})
		}
	}
	for _, s := range steps {
		if err := regSet(s[0], s[1], s[2]); err != nil {
			return err
		}
	}
	notifyAssocChanged()
	return nil
}

// restartExplorer перезапускает проводник: список пунктов нового меню
// Windows 11 он читает при старте и только что установленный пакет не видит.
func restartExplorer() {
	kill := exec.Command("taskkill.exe", "/f", "/im", "explorer.exe")
	kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	_ = kill.Run()
	// обычно Windows сама поднимает оболочку; если нет - запускаем
	for range 20 {
		time.Sleep(250 * time.Millisecond)
		if processRunning("explorer.exe") {
			return
		}
	}
	if cmd := exec.Command("explorer.exe"); cmd.Start() == nil {
		go cmd.Wait()
	}
}

func processRunning(name string) bool {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), name) {
			return true
		}
	}
	return false
}

// offerExplorerRestart: после установки меню Windows 11 предложить перезапуск проводника.
func offerExplorerRestart(owner walk.Form) {
	msg := "Пункт Raspak появится в меню Windows 11 после перезапуска проводника.\n\n" +
		"Перезапустить сейчас? Открытые окна папок закроются, панель задач на секунду пропадет."
	if walk.MsgBox(owner, appName, msg, walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) == win.IDYES {
		restartExplorer()
	}
}

func openDefaultApps() { shellOpen("ms-settings:defaultapps?registeredAppUser=" + appName) }

// ---------- открытие файлов и папок ----------

func shellOpen(target string) error {
	err := windows.ShellExecute(0, ptr("open"), ptr(target), nil, nil, windows.SW_SHOWNORMAL)
	if err != nil && fileExists(target) {
		// нет программы для этого типа - «Открыть с помощью»
		err = windows.ShellExecute(0, ptr("openas"), ptr(target), nil, nil, windows.SW_SHOWNORMAL)
	}
	return err
}

func ptr(s string) *uint16 {
	p, _ := windows.UTF16PtrFromString(s)
	return p
}

// showInExplorer открывает папку; sel - выделить файл или папку в родительской.
func showInExplorer(path string, sel bool) {
	path = filepath.Clean(path)
	if sel && fileExists(path) {
		cmd := exec.Command("explorer.exe")
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + path + `"`}
		if cmd.Start() == nil {
			go cmd.Wait()
		}
		return
	}
	for p := path; p != "" && p != filepath.Dir(p); p = filepath.Dir(p) {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			_ = shellOpen(p)
			return
		}
	}
}

// ---------- значки файлов ----------

var iconCache = map[string]*walk.Icon{}

// fileIcon - маленький значок проводника для типа файла (по расширению).
func fileIcon(name string, dir bool, dpi int) *walk.Icon {
	key := strings.ToLower(filepath.Ext(name))
	attrs := uint32(windows.FILE_ATTRIBUTE_NORMAL)
	query := "file" + key
	if dir {
		key, attrs, query = `\dir`, windows.FILE_ATTRIBUTE_DIRECTORY, "folder"
	}
	if ic, ok := iconCache[key]; ok {
		return ic
	}
	type shFileInfo struct {
		Icon        win.HICON
		IconIndex   int32
		Attributes  uint32
		DisplayName [260]uint16
		TypeName    [80]uint16
	}
	const shgfiIcon, shgfiSmallIcon, shgfiUseFileAttributes = 0x100, 0x1, 0x10
	var sfi shFileInfo
	r, _, _ := procSHGetFileInfoW.Call(uintptr(unsafe.Pointer(ptr(query))), uintptr(attrs),
		uintptr(unsafe.Pointer(&sfi)), unsafe.Sizeof(sfi), shgfiIcon|shgfiSmallIcon|shgfiUseFileAttributes)
	var ic *walk.Icon
	if r != 0 && sfi.Icon != 0 {
		ic, _ = walk.NewIconFromHICONForDPI(sfi.Icon, dpi)
	}
	iconCache[key] = ic
	return ic
}

// ---------- выбор папки ----------
// Системный диалог (IFileOpenDialog с FOS_PICKFOLDERS) - тот же, что в проводнике.

var (
	clsidFileOpenDialog = windows.GUID{Data1: 0xDC1C5A9C, Data2: 0xE88A, Data3: 0x4DDE, Data4: [8]byte{0xA5, 0xA1, 0x60, 0xF8, 0x2A, 0x20, 0xAE, 0xF7}}
	iidFileOpenDialog   = windows.GUID{Data1: 0xD57C7288, Data2: 0xD4AD, Data3: 0x4768, Data4: [8]byte{0xBE, 0x02, 0x9D, 0x96, 0x95, 0x32, 0xD9, 0x60}}
	iidShellItem        = windows.GUID{Data1: 0x43826D1E, Data2: 0xE718, Data3: 0x42EE, Data4: [8]byte{0xBC, 0x55, 0xA1, 0xE2, 0x61, 0xC3, 0x7B, 0xFE}}
)

// Номера методов в таблице COM-интерфейсов.
const (
	vRelease        = 2
	vShow           = 3
	vSetOptions     = 9
	vGetOptions     = 10
	vSetFolder      = 12
	vSetTitle       = 17
	vGetResult      = 20
	vGetDisplayName = 5 // IShellItem

	fosPickFolders     = 0x20
	fosForceFileSystem = 0x40
	fosNoChangeDir     = 0x8
	sigdnFileSysPath   = 0x80058000
	hresultCancelled   = 0x800704C7
	clsctxInprocServer = 1
	coinitApartment    = 0x2
	coinitDisableOle1  = 0x4
)

func comCall(obj unsafe.Pointer, n int, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(obj)
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(n)*unsafe.Sizeof(uintptr(0))))
	r, _, _ := syscall.SyscallN(fn, append([]uintptr{uintptr(obj)}, args...)...)
	return r
}

func hrErr(what string, hr uintptr) error {
	return errors.New(what + ": " + windows.Errno(hr&0xFFFF).Error())
}

// pickFolder: пустая строка без ошибки - пользователь нажал «Отмена».
// Вызывается из потока окон: диалог крутит свой модальный цикл сообщений.
func pickFolder(ownerForm walk.Form, title, initial string) (string, error) {
	var owner uintptr
	if ownerForm != nil {
		owner = uintptr(ownerForm.Handle())
	}
	return pickFolderHwnd(owner, title, initial)
}

// pickFolderHwnd - то же для окна, которое не из walk (WebView2).
func pickFolderHwnd(owner uintptr, title, initial string) (string, error) {
	if hr, _, _ := procCoInitializeEx.Call(0, coinitApartment|coinitDisableOle1); int32(hr) >= 0 {
		defer procCoUninitialize.Call()
	}
	var dlg unsafe.Pointer
	hr, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidFileOpenDialog)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidFileOpenDialog)), uintptr(unsafe.Pointer(&dlg)))
	if int32(hr) < 0 || dlg == nil {
		return "", hrErr("диалог выбора папки", hr)
	}
	defer comCall(dlg, vRelease)

	var opts uint32
	comCall(dlg, vGetOptions, uintptr(unsafe.Pointer(&opts)))
	comCall(dlg, vSetOptions, uintptr(opts|fosPickFolders|fosForceFileSystem|fosNoChangeDir))
	t := ptr(title)
	comCall(dlg, vSetTitle, uintptr(unsafe.Pointer(t)))
	for initial != "" && !fileExists(initial) && filepath.Dir(initial) != initial {
		initial = filepath.Dir(initial)
	}
	if initial != "" && fileExists(initial) {
		p := ptr(initial)
		var item unsafe.Pointer
		hr, _, _ := procSHCreateItemFromParsingName.Call(uintptr(unsafe.Pointer(p)), 0,
			uintptr(unsafe.Pointer(&iidShellItem)), uintptr(unsafe.Pointer(&item)))
		if int32(hr) >= 0 && item != nil {
			comCall(dlg, vSetFolder, uintptr(item))
			comCall(item, vRelease)
		}
		runtime.KeepAlive(p)
	}
	runtime.KeepAlive(t)

	hr = comCall(dlg, vShow, owner)
	if uint32(hr) == hresultCancelled {
		return "", nil
	}
	if int32(hr) < 0 {
		return "", hrErr("диалог выбора папки", hr)
	}
	var item unsafe.Pointer
	if hr := comCall(dlg, vGetResult, uintptr(unsafe.Pointer(&item))); int32(hr) < 0 || item == nil {
		return "", hrErr("выбранная папка", hr)
	}
	defer comCall(item, vRelease)
	var name *uint16
	if hr := comCall(item, vGetDisplayName, sigdnFileSysPath, uintptr(unsafe.Pointer(&name))); int32(hr) < 0 || name == nil {
		return "", errors.New("выбрана не папка на диске")
	}
	defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(name)))
	return windows.UTF16PtrToString(name), nil
}

func exePath() string {
	p, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p
}
