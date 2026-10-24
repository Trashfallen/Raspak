package main

// Встраивание в Windows и окно настроек.

import (
	"context"
	"errors"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
)

// applyIntegration включает или выключает меню проводника (классическое и
// Windows 11) и ассоциации архивов. Может идти несколько секунд (PowerShell).
func applyIntegration(cfg *Config, menu, assoc bool) error {
	exe := exePath()
	var errs []error
	if err := setContextMenu(exe, menu); err != nil {
		errs = append(errs, err)
	}
	if win11Menu() {
		if menu {
			if err := registerMenuPackage(exe, true); err != nil {
				errs = append(errs, err)
			} else {
				cfg.setMenuPkg(menuPkgStamp(exe))
			}
		} else if cfg.menuPkg() != "" {
			if err := unregisterMenuPackage(); err != nil {
				errs = append(errs, err)
			}
			cfg.setMenuPkg("")
		}
	}
	if err := setAssociations(exe, assoc); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// menuPkgStamp: пакет меню надо перерегистрировать, если сменилась версия или папка exe.
func menuPkgStamp(exe string) string { return menuPkgVersion + "|" + exe }

// syncIntegration при запуске обновляет пути к exe в реестре и пакет меню
// (программу могли перенести или обновить). Тихо, в фоне; если пакет надо
// переподписать (новые пункты меню), вызывает needResign - спросить человека.
func syncIntegration(cfg *Config, needResign func()) {
	if !cfg.ContextMenu && !cfg.Associations {
		return
	}
	go func() {
		exe := exePath()
		if cfg.ContextMenu {
			_ = setContextMenu(exe, true)
			if win11Menu() && cfg.menuPkg() != menuPkgStamp(exe) {
				switch err := registerMenuPackage(exe, false); {
				case err == nil:
					cfg.setMenuPkg(menuPkgStamp(exe))
				case errors.Is(err, errNeedResign) && needResign != nil:
					needResign()
				}
			}
		}
		if cfg.Associations {
			_ = setAssociations(exe, true)
		}
		_ = cfg.Save()
	}()
}

func (mw *mainWindow) settings() {
	var (
		dlg                           *walk.Dialog
		menuCB, assocCB, openCB, upCB *walk.CheckBox
		okBtn, cancelBtn              *walk.PushButton
		checkBtn                      *walk.PushButton
		menu, assoc, openAfter        bool
		checkUpd                      bool
	)
	cfg := mw.cfg
	section := func(t string) Label {
		return Label{Text: t, Font: Font{Family: "Segoe UI", PointSize: 10, Bold: true}}
	}
	menuText := "Пункты Raspak в меню архивов (правая кнопка мыши)"
	if win11Menu() {
		menuText = "Пункт «Raspak» в меню архивов (правая кнопка мыши)"
	}
	version := appVersion
	if version == "dev" {
		version = "dev (сборка без версии)"
	}
	res, err := Dialog{
		AssignTo:      &dlg,
		Title:         "Настройки - " + appName,
		Icon:          appIcon,
		MinSize:       Size{Width: 460},
		Layout:        VBox{Margins: Margins{Left: 16, Top: 12, Right: 16, Bottom: 12}, Spacing: 6},
		DefaultButton: &okBtn,
		CancelButton:  &cancelBtn,
		Children: []Widget{
			section("Проводник"),
			CheckBox{AssignTo: &menuCB, Text: menuText, Checked: cfg.ContextMenu},
			CheckBox{AssignTo: &assocCB, Text: "Открывать архивы двойным кликом", Checked: cfg.Associations},
			LinkLabel{
				Text:            `Если архивы открывает другая программа, <a id="apps">выберите Raspak в параметрах Windows</a>`,
				OnLinkActivated: func(*walk.LinkLabelLink) { openDefaultApps() },
			},
			VSpacer{Size: 6},
			section("Распаковка"),
			CheckBox{AssignTo: &openCB, Text: "Открывать папку после распаковки", Checked: cfg.OpenAfter},
			VSpacer{Size: 6},
			section("Обновления"),
			CheckBox{AssignTo: &upCB, Text: "Проверять новые версии при запуске", Checked: cfg.CheckUpdates},
			Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
				LinkLabel{
					Text:            `Версия ` + version + ` · <a id="gh">GitHub</a>`,
					OnLinkActivated: func(*walk.LinkLabelLink) { shellOpen("https://github.com/" + updateRepo) },
				},
				HSpacer{},
				PushButton{AssignTo: &checkBtn, Text: "Проверить сейчас", OnClicked: func() { mw.checkUpdateNow(dlg) }},
			}},
			VSpacer{Size: 8},
			Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
				HSpacer{},
				PushButton{AssignTo: &okBtn, Text: "OK", OnClicked: func() {
					// читать галочки, пока окно живо: после Run оно уничтожено и все читается как «выкл»
					menu, assoc = menuCB.Checked(), assocCB.Checked()
					openAfter, checkUpd = openCB.Checked(), upCB.Checked()
					dlg.Accept()
				}},
				PushButton{AssignTo: &cancelBtn, Text: "Отмена", OnClicked: func() { dlg.Cancel() }},
			}},
		},
	}.Run(mw)
	if err != nil || res != walk.DlgCmdOK {
		return
	}
	cfg.OpenAfter, cfg.CheckUpdates = openAfter, checkUpd
	if menu != cfg.ContextMenu || assoc != cfg.Associations {
		menuTurnedOn := menu && !cfg.ContextMenu
		cfg.ContextMenu, cfg.Associations, cfg.Asked = menu, assoc, true
		err := runTask(mw, "Настраиваю проводник", 300*time.Millisecond, func(context.Context, *Job, *Task) error {
			return applyIntegration(cfg, menu, assoc)
		})
		if err != nil {
			showError(mw, "Не все удалось настроить.", err)
		}
		_ = cfg.Save()
		if menuTurnedOn && win11Menu() && cfg.menuPkg() != "" {
			offerExplorerRestart(mw)
		}
	}
	_ = cfg.Save()
}

func (mw *mainWindow) checkUpdateNow(owner walk.Form) {
	var rel *release
	err := runTask(owner, "Проверяю обновления", 300*time.Millisecond, func(context.Context, *Job, *Task) error {
		var err error
		rel, err = checkUpdate()
		return err
	})
	switch {
	case err != nil:
		showError(owner, "Не удалось проверить обновления.", err)
	case rel == nil:
		walk.MsgBox(owner, appName, "Установлена последняя версия.", walk.MsgBoxIconInformation)
	default:
		mw.offerUpdate(rel)
		walk.MsgBox(owner, appName, "Доступна версия "+rel.Tag+". Кнопка «Обновить» появилась в окне.", walk.MsgBoxIconInformation)
	}
}
