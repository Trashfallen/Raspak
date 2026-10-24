package main

// Окна-диалоги: прогресс операции, пароль, файл уже существует.

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

var appIcon *walk.Icon

func loadAppIcon() {
	if ic, err := walk.NewIconFromResourceId(1); err == nil {
		appIcon = ic
	} else if ic, err := walk.NewIconFromResource("APP"); err == nil {
		appIcon = ic
	}
}

// Task - связь фоновой операции с окном прогресса.
type Task struct {
	dlg *walk.Dialog
}

// Owner - окно для вопросов во время операции.
func (t *Task) Owner() walk.Form { return t.dlg }

// Call выполняет f в потоке окон и ждет ее.
func (t *Task) Call(f func()) {
	done := make(chan struct{})
	t.dlg.Synchronize(func() {
		defer close(done)
		f()
	})
	<-done
}

// runTask выполняет fn в фоне с окном прогресса. Окно появляется, только если
// работа длится дольше delay. Закрытие окна или «Отмена» отменяют ctx.
func runTask(owner walk.Form, title string, delay time.Duration, fn func(ctx context.Context, job *Job, t *Task) error) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := &Job{}
	job.Total.Store(-1)

	var (
		dlg               *walk.Dialog
		head, file, stats *walk.Label
		bar               *walk.ProgressBar
		cancelBtn         *walk.PushButton
	)
	err := Dialog{
		AssignTo:     &dlg,
		Title:        appName,
		Icon:         appIcon,
		MinSize:      Size{Width: 480},
		Layout:       VBox{Margins: Margins{Left: 16, Top: 14, Right: 16, Bottom: 12}, Spacing: 8},
		CancelButton: &cancelBtn,
		Children: []Widget{
			Label{AssignTo: &head, Text: title, Font: Font{Family: "Segoe UI", PointSize: 11}, EllipsisMode: EllipsisEnd},
			Label{AssignTo: &file, Text: " ", EllipsisMode: EllipsisPath, TextColor: walk.RGB(96, 96, 96)},
			ProgressBar{AssignTo: &bar, MinValue: 0, MaxValue: 1000, MinSize: Size{Height: 18}},
			Composite{
				Layout: HBox{MarginsZero: true},
				Children: []Widget{
					Label{AssignTo: &stats, Text: " "},
					HSpacer{},
					PushButton{AssignTo: &cancelBtn, Text: "Отмена"},
				},
			},
		},
	}.Create(owner)
	if err != nil {
		return err
	}
	if owner == nil {
		// без главного окна (запуск из проводника) - своя кнопка на панели задач
		h := dlg.Handle()
		win.SetWindowLong(h, win.GWL_EXSTYLE, win.GetWindowLong(h, win.GWL_EXSTYLE)|win.WS_EX_APPWINDOW)
	}
	t := &Task{dlg: dlg}
	done := make(chan error, 1)
	go func() { done <- fn(ctx, job, t) }()

	select {
	case err := <-done:
		dlg.Dispose()
		return err
	case <-time.After(delay):
	}

	var (
		result   error
		finished bool
		closed   bool
		start    = time.Now()
		samples  []sample
	)
	cancelBtn.Clicked().Attach(func() { dlg.Close(walk.DlgCmdCancel) })
	dlg.Closing().Attach(func(canceled *bool, _ walk.CloseReason) {
		if !finished {
			*canceled = true
			cancel()
			cancelBtn.SetEnabled(false)
			cancelBtn.SetText("Отменяю…")
		}
	})
	update := func() {
		if closed {
			return
		}
		if s := job.Current(); s != "" {
			file.SetText(strings.ReplaceAll(s, "/", `\`))
		}
		done, total := job.Done.Load(), job.Total.Load()
		var frac float64 = -1
		cur, all := done, total
		if total <= 0 {
			if rt := job.ReadTotal.Load(); rt > 0 {
				cur, all = job.Read.Load(), rt
				frac = min(float64(cur)/float64(all), 1)
			}
		} else {
			frac = min(float64(done)/float64(total), 1)
		}
		if frac < 0 {
			if !bar.MarqueeMode() {
				bar.SetMarqueeMode(true)
			}
		} else {
			if bar.MarqueeMode() {
				bar.SetMarqueeMode(false)
			}
			bar.SetValue(int(frac * 1000))
		}
		now := time.Now()
		samples = append(samples, sample{now, cur})
		for len(samples) > 2 && now.Sub(samples[0].t) > 3*time.Second {
			samples = samples[1:]
		}
		var speed float64
		if d := now.Sub(samples[0].t).Seconds(); d > 0.3 {
			speed = float64(cur-samples[0].n) / d
		}
		parts := []string{}
		if total > 0 {
			parts = append(parts, formatSize(done)+" из "+formatSize(total))
		} else if done > 0 {
			parts = append(parts, formatSize(done))
		}
		if speed > 0 && now.Sub(start) > time.Second {
			parts = append(parts, formatSize(int64(speed))+"/с")
			if frac > 0 && all > cur {
				parts = append(parts, "осталось "+formatDuration(time.Duration(float64(all-cur)/speed*float64(time.Second))))
			}
		}
		stats.SetText(strings.Join(parts, " · "))
		if frac >= 0 {
			dlg.SetTitle(fmt.Sprintf("%d%% - %s", int(frac*100), appName))
		}
	}
	update()

	stop := make(chan struct{})
	go func() {
		tk := time.NewTicker(200 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				dlg.Synchronize(update)
			}
		}
	}()
	go func() {
		err := <-done
		dlg.Synchronize(func() {
			finished, result = true, err
			dlg.Close(walk.DlgCmdOK)
		})
	}()
	dlg.Run()
	closed = true
	close(stop)
	dlg.Dispose()
	return result
}

type sample struct {
	t time.Time
	n int64
}

func formatDuration(d time.Duration) string {
	s := int(d.Seconds())
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// askPassword: ok=false - пользователь отказался.
func askPassword(owner walk.Form, name string, wrong bool) (string, bool) {
	var (
		dlg      *walk.Dialog
		edit     *walk.LineEdit
		okBtn    *walk.PushButton
		cancelBt *walk.PushButton
	)
	text := "Архив «" + name + "» защищен паролем."
	if wrong {
		text = "Пароль не подошел к «" + name + "». Попробуйте еще раз."
	}
	res, err := Dialog{
		AssignTo:      &dlg,
		Title:         "Пароль - " + appName,
		Icon:          appIcon,
		MinSize:       Size{Width: 400},
		Layout:        VBox{Margins: Margins{Left: 16, Top: 14, Right: 16, Bottom: 12}, Spacing: 8},
		DefaultButton: &okBtn,
		CancelButton:  &cancelBt,
		Children: []Widget{
			Label{Text: text},
			LineEdit{AssignTo: &edit, PasswordMode: true},
			CheckBox{Text: "Показать пароль", OnCheckedChanged: func() { edit.SetPasswordMode(!edit.PasswordMode()) }},
			Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
				HSpacer{},
				PushButton{AssignTo: &okBtn, Text: "OK", OnClicked: func() { dlg.Accept() }},
				PushButton{AssignTo: &cancelBt, Text: "Отмена", OnClicked: func() { dlg.Cancel() }},
			}},
		},
	}.Run(owner)
	if err != nil || res != walk.DlgCmdOK {
		return "", false
	}
	return edit.Text(), true
}

// askConflict: файл уже есть на диске.
func askConflict(owner walk.Form, target string, e Entry, old os.FileInfo) (Conflict, bool) {
	var (
		dlg *walk.Dialog
		all *walk.CheckBox
	)
	choice := ConflictCancel
	pick := func(c Conflict) func() {
		return func() { choice = c; dlg.Accept() }
	}
	describe := func(size int64, t time.Time) string {
		s := formatSize(size)
		if !t.IsZero() {
			s += ", " + t.Local().Format("02.01.2006 15:04")
		}
		return s
	}
	_, err := Dialog{
		AssignTo: &dlg,
		Title:    "Файл уже существует - " + appName,
		Icon:     appIcon,
		MinSize:  Size{Width: 460},
		Layout:   VBox{Margins: Margins{Left: 16, Top: 14, Right: 16, Bottom: 12}, Spacing: 6},
		Children: []Widget{
			Label{Text: "Такой файл уже есть:", Font: Font{Family: "Segoe UI", PointSize: 10}},
			Label{Text: target, EllipsisMode: EllipsisPath},
			Label{Text: "На диске: " + describe(old.Size(), old.ModTime())},
			Label{Text: "В архиве: " + describe(e.Size, e.Mod)},
			CheckBox{AssignTo: &all, Text: "Для всех таких файлов"},
			Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
				HSpacer{},
				PushButton{Text: "Заменить", OnClicked: pick(ConflictReplace)},
				PushButton{Text: "Пропустить", OnClicked: pick(ConflictSkip)},
				PushButton{Text: "Отмена", OnClicked: pick(ConflictCancel)},
			}},
		},
	}.Run(owner)
	if err != nil {
		return ConflictCancel, false
	}
	return choice, all.Checked()
}

func showError(owner walk.Form, title string, err error) {
	walk.MsgBox(owner, appName, title+"\n\n"+userError(err), walk.MsgBoxIconError)
}

// isPasswordErr: стоит спросить пароль (снова).
func isPasswordErr(err error) bool {
	return errors.Is(err, errNeedPassword) || errors.Is(err, errBadPassword)
}

// openWithPassword открывает архив, спрашивая пароль, пока он нужен.
// tryFirst - пароль, который стоит попробовать молча (прошлый удачный).
func openWithPassword(owner walk.Form, path, tryFirst string) (*Archive, error) {
	size := sourceSize(path)
	name := displayName(path)
	pw, wrong := "", false
	tried := tryFirst == ""
	for {
		var a *Archive
		err := runTask(owner, "Открываю «"+name+"»", 400*time.Millisecond, func(ctx context.Context, job *Job, _ *Task) error {
			job.ReadTotal.Store(size)
			var err error
			a, err = OpenArchive(ctx, path, pw, &job.Read)
			return err
		})
		if err == nil {
			return a, nil
		}
		if !isPasswordErr(err) {
			return nil, err
		}
		if !tried {
			tried, pw = true, tryFirst
			continue
		}
		wrong = pw != ""
		p, ok := askPassword(owner, name, wrong)
		if !ok {
			return nil, context.Canceled
		}
		pw = p
	}
}

// extractWithPassword распаковывает, спрашивая пароль, если он понадобился только сейчас.
func extractWithPassword(owner walk.Form, a *Archive, title string, opt ExtractOptions) (ExtractResult, error) {
	for {
		var res ExtractResult
		err := runTask(owner, title, 300*time.Millisecond, func(ctx context.Context, job *Job, t *Task) error {
			o := opt
			if o.Ask == nil && o.Conflict == ConflictAsk {
				o.Ask = func(target string, e Entry, old os.FileInfo) (c Conflict, all bool) {
					t.Call(func() { c, all = askConflict(t.Owner(), target, e, old) })
					return
				}
			}
			if a.single {
				job.ReadTotal.Store(a.Size)
			}
			var err error
			res, err = Extract(ctx, a, o, job)
			return err
		})
		if !isPasswordErr(err) {
			return res, err
		}
		p, ok := askPassword(owner, a.Name, a.Password != "")
		if !ok {
			return res, context.Canceled
		}
		a.Password = p
	}
}

func sourceSize(path string) int64 {
	s, err := openSource(firstVolume(path), nil)
	if err != nil {
		return 0
	}
	defer s.Close()
	return s.Size()
}

func displayName(path string) string {
	n := filepath.Base(path)
	if volumeRe.MatchString(n) {
		return volumeRe.ReplaceAllString(n, "")
	}
	return n
}
