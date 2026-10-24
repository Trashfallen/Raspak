package main

// Распаковка из меню проводника: «Raspak.exe --extract a.zip b.rar».
//
// Если выделить 10 архивов, проводник может запустить 10 копий программы.
// Чтобы не было 10 окон сразу, копии складывают задания в общую очередь
// (queue.txt под замком queue.lock), а разбирает ее одна ведущая копия
// (держит leader.lock). Ведущая отпускает замок только под замком очереди,
// убедившись, что очередь пуста, - так задание не потеряется.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lxn/walk"
	"golang.org/x/sys/windows"
)

type batchItem struct {
	mode string // extract, extract-to, test
	path string
}

// lockFile открывает файл без общего доступа: пока он открыт, замок наш.
func lockFile(path string, wait time.Duration) (windows.Handle, error) {
	deadline := time.Now().Add(wait)
	for {
		h, err := windows.CreateFile(ptr(path), windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil,
			windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err == nil {
			return h, nil
		}
		if err != windows.ERROR_SHARING_VIOLATION || time.Now().After(deadline) {
			return 0, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// withQueue выполняет f под замком очереди.
func withQueue(f func(path string) error) error {
	if err := os.MkdirAll(dataDir(), 0o755); err != nil {
		return err
	}
	h, err := lockFile(filepath.Join(dataDir(), "queue.lock"), 10*time.Second)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return f(filepath.Join(dataDir(), "queue.txt"))
}

func pushQueue(items []batchItem) error {
	return withQueue(func(q string) error {
		f, err := os.OpenFile(q, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		for _, it := range items {
			fmt.Fprintf(f, "%s\t%s\n", it.mode, it.path)
		}
		return nil
	})
}

// takeQueue забирает все задания; если их нет - отпускает ведущий замок.
func takeQueue(leader *windows.Handle) []batchItem {
	var items []batchItem
	err := withQueue(func(q string) error {
		f, err := os.Open(q)
		if err == nil {
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 64<<10), 1<<20)
			for sc.Scan() {
				if mode, path, ok := strings.Cut(sc.Text(), "\t"); ok {
					items = append(items, batchItem{mode, path})
				}
			}
			f.Close()
			os.Remove(q)
		}
		if len(items) == 0 {
			windows.CloseHandle(*leader)
			*leader = 0
		}
		return nil
	})
	if err != nil && *leader != 0 {
		windows.CloseHandle(*leader)
		*leader = 0
	}
	return items
}

func runBatch(mode string, files []string) {
	var items []batchItem
	for _, f := range files {
		if abs, err := filepath.Abs(f); err == nil {
			f = abs
		}
		items = append(items, batchItem{mode, f})
	}
	if err := pushQueue(items); err != nil {
		// очередь недоступна - работаем сами
		processBatch(items)
		return
	}
	leader, err := lockFile(filepath.Join(dataDir(), "leader.lock"), 0)
	if err != nil {
		return // задания заберет уже работающая копия
	}
	b := &batchState{}
	for leader != 0 {
		items := takeQueue(&leader)
		if len(items) == 0 {
			break
		}
		b.run(items)
		if b.cancelled {
			// отмена - остальное из очереди тоже не делаем
			for leader != 0 && len(takeQueue(&leader)) > 0 {
			}
		}
	}
	b.report()
}

func processBatch(items []batchItem) {
	b := &batchState{}
	b.run(items)
	b.report()
}

type batchState struct {
	done      int
	pw        string // последний подошедший пароль: тома и серии архивов часто с одним паролем
	dest      string // для «Распаковать в…» - спрашиваем один раз
	errs      []string
	tested    []string
	cancelled bool
}

func (b *batchState) run(items []batchItem) {
	for i, it := range items {
		if b.cancelled {
			return
		}
		n := fmt.Sprintf(" (%d из %d)", b.done+i+1, b.done+len(items))
		if len(items) == 1 && b.done == 0 {
			n = ""
		}
		b.one(it, n)
	}
	b.done += len(items)
}

func (b *batchState) one(it batchItem, counter string) {
	fail := func(err error) {
		if errors.Is(err, context.Canceled) {
			b.cancelled = true
			return
		}
		b.errs = append(b.errs, filepath.Base(it.path)+": "+userError(err))
	}
	base := filepath.Dir(it.path)
	if it.mode == "extract-to" {
		if b.dest == "" {
			p, err := pickFolder(nil, "Куда распаковать «"+filepath.Base(it.path)+"»", base)
			if err != nil || p == "" {
				b.cancelled = true
				return
			}
			b.dest = p
		}
		base = b.dest
	}
	a, err := openWithPassword(nil, it.path, b.pw)
	if err != nil {
		fail(err)
		return
	}
	if it.mode == "test" {
		res, err := extractWithPassword(nil, a, "Проверка «"+a.Name+"»"+counter, ExtractOptions{Test: true})
		if err != nil {
			fail(err)
			return
		}
		b.pw = a.Password
		b.tested = append(b.tested, fmt.Sprintf("%s: %s, ошибок нет", a.Name, plural(res.Files, "файл", "файла", "файлов")))
		return
	}
	dest, result := smartDest(a, base)
	res, err := extractWithPassword(nil, a, "Распаковка «"+a.Name+"»"+counter, ExtractOptions{Dest: dest})
	if err != nil {
		fail(err)
		return
	}
	b.pw = a.Password
	if len(res.Skipped) > 0 {
		walk.MsgBox(nil, appName, a.Name+"\n\n"+skippedText(res.Skipped), walk.MsgBoxIconWarning)
	}
	if it.mode == "extract-to" && loadConfig().OpenAfter {
		showInExplorer(result, true)
	}
}

func (b *batchState) report() {
	switch {
	case len(b.errs) > 0:
		walk.MsgBox(nil, appName, "Не получилось:\n\n"+strings.Join(b.errs, "\n\n"), walk.MsgBoxIconError)
	case len(b.tested) > 0 && !b.cancelled:
		walk.MsgBox(nil, appName, strings.Join(b.tested, "\n"), walk.MsgBoxIconInformation)
	}
}
