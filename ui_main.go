package main

// Главное окно: содержимое архива по папкам, распаковка всего или выбранного,
// проверка, открытие файла из архива двойным кликом.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
)

type mainWindow struct {
	*walk.MainWindow
	cfg *Config

	tv      *walk.TableView
	model   *listModel
	empty   *walk.Composite
	destRow *walk.Composite
	info    *walk.Label // имя архива, папка внутри, сводка или итог распаковки
	upBtn   *walk.PushButton
	updBtn  *walk.PushButton
	dest    *walk.LineEdit

	arc       *Archive
	tree      tree
	cwd       string // текущая папка в архиве
	suggested string // предложенная папка распаковки; пока поле не меняли - «умная» распаковка
	homeDir   string // где лежит архив (у вложенного - папка внешнего)
	update    *release
}

func runMain(cfg *Config, files []string) {
	mw := &mainWindow{cfg: cfg, model: &listModel{}}
	err := MainWindow{
		AssignTo:    &mw.MainWindow,
		Title:       appName,
		Icon:        appIcon,
		Visible:     false,
		Size:        Size{Width: 600, Height: 420},
		MinSize:     Size{Width: 420, Height: 300},
		Layout:      VBox{Margins: Margins{Left: 10, Top: 8, Right: 10, Bottom: 10}, Spacing: 8},
		OnDropFiles: mw.onDrop,
		Children: []Widget{
			Composite{
				Layout: HBox{MarginsZero: true, Spacing: 6},
				Children: []Widget{
					PushButton{Text: "Открыть", MaxSize: Size{Width: 90}, OnClicked: mw.openDialog},
					PushButton{AssignTo: &mw.upBtn, Text: "←", Visible: false, MaxSize: Size{Width: 30}, ToolTipText: "Назад (Backspace)", OnClicked: mw.goUp},
					Label{AssignTo: &mw.info, EllipsisMode: EllipsisEnd, TextColor: walk.RGB(100, 100, 100), StretchFactor: 1},
					HSpacer{},
					PushButton{AssignTo: &mw.updBtn, Visible: false, OnClicked: mw.installUpdate},
					PushButton{Text: "⚙", MaxSize: Size{Width: 30}, ToolTipText: "Настройки", OnClicked: mw.settings},
				},
			},
			Composite{
				AssignTo: &mw.empty,
				Layout:   VBox{},
				Children: []Widget{
					VSpacer{},
					Label{Text: "Перетащите архив сюда", Font: Font{Family: "Segoe UI", PointSize: 13}, TextAlignment: AlignCenter},
					Label{Text: "zip, 7z, rar, tar, gz, xz, bz2, zst", TextAlignment: AlignCenter, TextColor: walk.RGB(128, 128, 128)},
					VSpacer{},
				},
			},
			TableView{
				AssignTo:       &mw.tv,
				Visible:        false,
				Model:          mw.model,
				MultiSelection: true,
				Columns: []TableViewColumn{
					{Title: "Имя", Width: 400},
					{Title: "Размер", Width: 80, Alignment: AlignFar},
				},
				OnItemActivated: mw.activate,
				ContextMenuItems: []MenuItem{
					Action{Text: "Открыть", OnTriggered: mw.activate},
					Action{Text: "Распаковать выбранное", OnTriggered: func() { mw.extractSelected(false) }},
					Action{Text: "Распаковать выбранное в…", OnTriggered: func() { mw.extractSelected(true) }},
					Separator{},
					Action{Text: "Проверить архив", OnTriggered: mw.test},
				},
			},
			Composite{
				AssignTo: &mw.destRow,
				Visible:  false,
				Layout:   HBox{MarginsZero: true, Spacing: 6},
				Children: []Widget{
					LineEdit{AssignTo: &mw.dest, ToolTipText: "Куда распаковать", OnKeyPress: func(k walk.Key) {
						if k == walk.KeyReturn {
							mw.extractAll()
						}
					}},
					PushButton{Text: "…", MaxSize: Size{Width: 30}, ToolTipText: "Выбрать папку", OnClicked: mw.browseDest},
					PushButton{Text: "Распаковать", OnClicked: mw.extractAll},
				},
			},
		},
	}.Create()
	if err != nil {
		walk.MsgBox(nil, appName, "Не удалось создать окно: "+err.Error(), walk.MsgBoxIconError)
		return
	}
	mw.model.dpi = mw.DPI()
	mw.tv.SizeChanged().Attach(func() { mw.Synchronize(mw.fitColumns) })
	mw.tv.KeyDown().Attach(func(k walk.Key) {
		if k == walk.KeyBack {
			mw.goUp()
		}
	})
	mw.Closing().Attach(func(*bool, walk.CloseReason) { mw.savePlacement() })
	open := walk.NewAction()
	open.SetShortcut(walk.Shortcut{Modifiers: walk.ModControl, Key: walk.KeyO})
	open.Triggered().Attach(mw.openDialog)
	mw.ShortcutActions().Add(open)
	mw.restorePlacement()

	if files = uniqueVolumes(files); len(files) > 1 {
		mw.Synchronize(func() { mw.batchExtract(files) })
	} else if len(files) == 1 {
		mw.Synchronize(func() { mw.open(files[0]) })
	}
	mw.Synchronize(mw.firstRun)
	if cfg.CheckUpdates && appVersion != "dev" {
		go func() {
			time.Sleep(3 * time.Second)
			if rel, err := checkUpdate(); err == nil && rel != nil {
				mw.Synchronize(func() { mw.offerUpdate(rel) })
			}
		}()
	}
	mw.Run()
}

// ---------- открытие и навигация ----------

func (mw *mainWindow) openDialog() {
	dlg := walk.FileDialog{
		Title:  "Открыть архив",
		Filter: "Архивы|*" + strings.Join(archiveExts, ";*") + ";*.jar;*.apk;*.cbz;*.cbr|Все файлы (*.*)|*.*",
	}
	if mw.homeDir != "" {
		dlg.InitialDirPath = mw.homeDir
	}
	if ok, _ := dlg.ShowOpen(mw); ok {
		mw.open(dlg.FilePath)
	}
}

func (mw *mainWindow) onDrop(files []string) {
	files = uniqueVolumes(files) // все тома одного архива - это один архив
	if len(files) == 1 {
		mw.open(files[0])
		return
	}
	mw.batchExtract(files)
}

func (mw *mainWindow) open(path string) {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	a, err := openWithPassword(mw, path, "")
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			showError(mw, "Не удалось открыть «"+filepath.Base(path)+"».", err)
		}
		return
	}
	nested := strings.HasPrefix(strings.ToLower(path), strings.ToLower(tempRoot()))
	if !nested {
		mw.homeDir = filepath.Dir(path)
	} else if mw.homeDir == "" {
		mw.homeDir = knownDesktop()
	}
	mw.arc, mw.tree, mw.cwd = a, buildTree(a.Entries), ""
	mw.empty.SetVisible(false)
	mw.tv.SetVisible(true)
	mw.destRow.SetVisible(true)
	mw.SetTitle(a.Name + " - " + appName)
	_, result := smartDest(a, mw.homeDir)
	mw.suggested = result
	mw.dest.SetText(result)
	mw.showDir("")
	mw.tv.SetFocus()
	mw.Synchronize(mw.fitColumns) // таблица только что стала видимой
}

func (mw *mainWindow) showDir(dir string) {
	mw.cwd = dir
	mw.model.setRows(mw.tree[dir])
	mw.upBtn.SetVisible(dir != "")
	mw.setStatus()
	if len(mw.model.rows) > 0 {
		mw.tv.SetCurrentIndex(0)
	}
}

// fitColumns: «Имя» занимает всю ширину, кроме «Размера».
func (mw *mainWindow) fitColumns() {
	cols := mw.tv.Columns()
	if cols.Len() < 2 {
		return
	}
	// размеры в единицах 96 DPI; 22 - полоса прокрутки и рамка
	if w := mw.tv.ClientBounds().Width - cols.At(1).Width() - 22; w > 100 {
		cols.At(0).SetWidth(w)
	}
}

func (mw *mainWindow) goUp() {
	if mw.arc == nil || mw.cwd == "" {
		return
	}
	from := mw.cwd
	parent, _ := splitPath(mw.cwd)
	mw.showDir(parent)
	for i, n := range mw.model.rows {
		if n.path == from {
			mw.tv.SetCurrentIndex(i)
			break
		}
	}
}

// setStatus: в папке архива - путь к ней, в корне - сводка по архиву.
func (mw *mainWindow) setStatus() {
	a := mw.arc
	if mw.cwd != "" {
		mw.info.SetText(strings.ReplaceAll(mw.cwd, "/", ` \ `))
		return
	}
	var files, dirs int
	var size, packed int64
	unknown := false
	for _, e := range a.Entries {
		if e.Dir {
			dirs++
			continue
		}
		files++
		if e.Size < 0 {
			unknown = true
		} else {
			size += e.Size
		}
		if e.Packed > 0 {
			packed += e.Packed
		}
	}
	parts := []string{plural(files, "файл", "файла", "файлов")}
	if dirs > 0 {
		parts[0] += ", " + plural(dirs, "папка", "папки", "папок")
	}
	if !unknown {
		parts = append(parts, formatSize(size))
	}
	parts = append(parts, a.Kind)
	if a.Encrypted || a.Password != "" {
		parts = append(parts, "с паролем")
	}
	mw.info.SetText(strings.Join(parts, " · "))
}

func plural(n int, one, few, many string) string {
	w := many
	if n%10 == 1 && n%100 != 11 {
		w = one
	} else if n%10 >= 2 && n%10 <= 4 && (n%100 < 12 || n%100 > 14) {
		w = few
	}
	return fmt.Sprintf("%d %s", n, w)
}

// activate: папка - войти, файл - распаковать во временную папку и открыть.
func (mw *mainWindow) activate() {
	i := mw.tv.CurrentIndex()
	if mw.arc == nil || i < 0 || i >= len(mw.model.rows) {
		return
	}
	n := mw.model.rows[i]
	if n.dir {
		mw.showDir(n.path)
		return
	}
	dir := filepath.Join(tempRoot(), fmt.Sprintf("%d", time.Now().UnixNano()))
	parent, _ := splitPath(n.path)
	_, err := extractWithPassword(mw, mw.arc, "Открываю «"+n.name+"»", ExtractOptions{
		Dest: dir, Select: []string{n.path}, Base: parent, Conflict: ConflictReplace,
	})
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			showError(mw, "Не удалось открыть «"+n.name+"».", err)
		}
		mw.setStatus()
		return
	}
	mw.setStatus()
	target := filepath.Join(dir, winSafeName(n.name))
	if isArchiveName(n.name) {
		mw.open(target) // архив в архиве - открываем здесь же
		return
	}
	if err := shellOpen(target); err != nil {
		showError(mw, "Не удалось открыть «"+n.name+"».", err)
	}
}

func isArchiveName(name string) bool {
	l := strings.ToLower(name)
	for _, e := range archiveExts {
		if strings.HasSuffix(l, e) {
			return true
		}
	}
	return false
}

// ---------- распаковка ----------

func (mw *mainWindow) browseDest() {
	if p, err := pickFolder(mw, "Куда распаковать", mw.dest.Text()); err != nil {
		showError(mw, "Не удалось выбрать папку.", err)
	} else if p != "" {
		mw.dest.SetText(p)
	}
}

// extractAll: если поле не меняли - «умная» папка рядом с архивом.
func (mw *mainWindow) extractAll() {
	if mw.arc == nil {
		return
	}
	text := strings.TrimSpace(mw.dest.Text())
	var dest, result string
	if text == mw.suggested {
		dest, result = smartDest(mw.arc, mw.homeDir)
	} else {
		if text == "" || !filepath.IsAbs(text) {
			walk.MsgBox(mw, appName, "Укажите полный путь к папке, например D:\\Распакованное.", walk.MsgBoxIconInformation)
			return
		}
		dest, result = text, text
	}
	mw.doExtract(ExtractOptions{Dest: dest}, result, dest != result)
}

func (mw *mainWindow) extractSelected(ask bool) {
	if mw.arc == nil {
		return
	}
	var sel []string
	for _, i := range mw.tv.SelectedIndexes() {
		sel = append(sel, mw.model.rows[i].path)
	}
	if len(sel) == 0 {
		return
	}
	dest := strings.TrimSpace(mw.dest.Text())
	if ask || !filepath.IsAbs(dest) {
		p, err := pickFolder(mw, "Куда распаковать выбранное", dest)
		if err != nil || p == "" {
			return
		}
		dest = p
	}
	mw.doExtract(ExtractOptions{Dest: dest, Select: sel, Base: mw.cwd}, dest, false)
}

// doExtract: result - что показать в проводнике; sel - выделить его в родительской папке.
func (mw *mainWindow) doExtract(opt ExtractOptions, result string, sel bool) {
	res, err := extractWithPassword(mw, mw.arc, "Распаковка «"+mw.arc.Name+"»", opt)
	mw.setStatus()
	if err != nil {
		if errors.Is(err, context.Canceled) {
			mw.info.SetText("Распаковка отменена")
		} else {
			showError(mw, "Не удалось распаковать «"+mw.arc.Name+"».", err)
		}
		return
	}
	mw.info.SetText(fmt.Sprintf("Готово: %s, %s", plural(res.Files, "файл", "файла", "файлов"), formatSize(res.Bytes)))
	if len(res.Skipped) > 0 {
		walk.MsgBox(mw, appName, skippedText(res.Skipped), walk.MsgBoxIconWarning)
	}
	if mw.cfg.OpenAfter {
		showInExplorer(result, sel || result != opt.Dest)
	}
}

func skippedText(s []string) string {
	text := "Пропущено (" + fmt.Sprint(len(s)) + "):\n"
	for i, x := range s {
		if i == 15 {
			text += fmt.Sprintf("… и еще %d", len(s)-15)
			break
		}
		text += "• " + x + "\n"
	}
	return text
}

func (mw *mainWindow) test() {
	if mw.arc == nil {
		return
	}
	res, err := extractWithPassword(mw, mw.arc, "Проверка «"+mw.arc.Name+"»", ExtractOptions{Test: true})
	mw.setStatus()
	switch {
	case errors.Is(err, context.Canceled):
	case err != nil:
		showError(mw, "Архив «"+mw.arc.Name+"» поврежден.", err)
	default:
		walk.MsgBox(mw, appName, fmt.Sprintf("Ошибок нет.\n\n%s, %s проверено.",
			plural(res.Files, "файл", "файла", "файлов"), formatSize(res.Bytes)), walk.MsgBoxIconInformation)
	}
}

// batchExtract: несколько архивов - каждый рядом с собой.
func (mw *mainWindow) batchExtract(files []string) {
	msg := fmt.Sprintf("Распаковать %s, каждый рядом с собой?", plural(len(files), "архив", "архива", "архивов"))
	if walk.MsgBox(mw, appName, msg, walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != win.IDYES {
		return
	}
	var errs []string
	pw := ""
	for i, f := range files {
		a, err := openWithPassword(mw, f, pw)
		if err == nil {
			pw = a.Password
			dest, _ := smartDest(a, filepath.Dir(f))
			_, err = extractWithPassword(mw, a, fmt.Sprintf("Распаковка «%s» (%d из %d)", a.Name, i+1, len(files)), ExtractOptions{Dest: dest})
		}
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			errs = append(errs, filepath.Base(f)+": "+userError(err))
		}
	}
	if len(errs) > 0 {
		walk.MsgBox(mw, appName, "Не все архивы распакованы:\n\n"+strings.Join(errs, "\n"), walk.MsgBoxIconWarning)
	} else {
		mw.info.SetText("Распаковано архивов: " + fmt.Sprint(len(files)))
	}
}

// ---------- окно ----------

func (mw *mainWindow) savePlacement() {
	var wp win.WINDOWPLACEMENT
	wp.Length = uint32(unsafeSizeofPlacement)
	if win.GetWindowPlacement(mw.Handle(), &wp) {
		r := wp.RcNormalPosition
		mw.cfg.Window.X, mw.cfg.Window.Y = int(r.Left), int(r.Top)
		mw.cfg.Window.W, mw.cfg.Window.H = int(r.Right-r.Left), int(r.Bottom-r.Top)
		mw.cfg.Window.Maximized = wp.ShowCmd == win.SW_SHOWMAXIMIZED
	}
	_ = mw.cfg.Save()
}

func (mw *mainWindow) restorePlacement() {
	w := mw.cfg.Window
	if w.W >= 400 && w.H >= 300 {
		mw.SetBoundsPixels(walk.Rectangle{X: w.X, Y: w.Y, Width: w.W, Height: w.H})
	}
	mw.Show()
	if w.Maximized {
		win.ShowWindow(mw.Handle(), win.SW_MAXIMIZE)
	}
	mw.fitColumns()
	win.SetForegroundWindow(mw.Handle())
}

// firstRun: один раз спрашиваем про встраивание в проводник.
func (mw *mainWindow) firstRun() {
	if mw.cfg.Asked || appVersion == "dev" {
		syncIntegration(mw.cfg)
		return
	}
	mw.cfg.Asked = true
	msg := "Встроить Raspak в Windows?\n\n" +
		"• пункты Raspak в меню архивов (правая кнопка мыши)\n" +
		"• открытие архивов двойным кликом\n\n" +
		"Это можно изменить в настройках."
	if walk.MsgBox(mw, appName, msg, walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) == win.IDYES {
		mw.cfg.ContextMenu, mw.cfg.Associations = true, true
		err := runTask(mw, "Встраиваю в проводник", 300*time.Millisecond, func(context.Context, *Job, *Task) error {
			return applyIntegration(mw.cfg, true, true)
		})
		if err != nil {
			showError(mw, "Не все удалось встроить.", err)
		}
	}
	_ = mw.cfg.Save()
}

// ---------- обновление ----------

func (mw *mainWindow) offerUpdate(rel *release) {
	mw.update = rel
	mw.updBtn.SetText("Обновить до " + rel.Tag)
	mw.updBtn.SetVisible(true)
}

func (mw *mainWindow) installUpdate() {
	rel := mw.update
	if rel == nil {
		return
	}
	if walk.MsgBox(mw, appName, "Установить "+rel.Tag+"? Программа перезапустится.", walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != win.IDYES {
		return
	}
	var newExe string
	err := runTask(mw, "Скачиваю "+rel.Tag, 0, func(ctx context.Context, job *Job, _ *Task) error {
		var err error
		newExe, err = downloadUpdate(ctx, rel, job)
		return err
	})
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			showError(mw, "Обновление не скачалось.", err)
		}
		return
	}
	var args []string
	if mw.arc != nil && !strings.HasPrefix(strings.ToLower(mw.arc.Path), strings.ToLower(tempRoot())) {
		args = append(args, mw.arc.Path)
	}
	if err := installAndRestart(newExe, args); err != nil {
		showError(mw, "Обновление не установилось.", err)
		return
	}
	mw.savePlacement()
	os.Exit(0)
}
