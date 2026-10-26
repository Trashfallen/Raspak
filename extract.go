package main

// Распаковка и проверка архива: безопасные имена для Windows, «умная» папка
// назначения, конфликты с существующими файлами, прогресс и отмена.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"
)

// Job - счетчики выполняемой операции, их читает окно прогресса.
type Job struct {
	Done      atomic.Int64 // байт распаковано
	Total     atomic.Int64 // всего байт; -1 - неизвестно
	Read      atomic.Int64 // байт прочитано из архива (прогресс одиночных сжатых файлов)
	Files     atomic.Int64
	ReadTotal atomic.Int64 // размер архива на диске (для прогресса по Read)
	cur       atomic.Value // string - текущий файл
	Phase     atomic.Value // string - что делаем, для заголовка окна
}

func (j *Job) Current() string {
	s, _ := j.cur.Load().(string)
	return s
}

func (j *Job) setCurrent(s string) { j.cur.Store(s) }

type Conflict int

const (
	ConflictAsk Conflict = iota
	ConflictReplace
	ConflictSkip
	ConflictCancel
)

type ExtractOptions struct {
	Dest     string
	Select   []string // пути в архиве (файлы и папки); пусто - все
	Base     string   // папка в архиве, относительно которой раскладывать выбранное
	Test     bool     // только проверить: прочитать все и сверить контрольные суммы
	Conflict Conflict
	// Ask спрашивает, что делать с существующим файлом; all - запомнить для остальных.
	Ask func(target string, e Entry, old os.FileInfo) (c Conflict, all bool)
}

type ExtractResult struct {
	Files, Dirs int
	Bytes       int64
	Existing    int      // пропущено существующих
	Skipped     []string // ссылки и опасные имена
}

// writeError - ошибка записи на диск (а не чтения архива).
type writeError struct {
	path string
	err  error
}

func (w *writeError) Error() string { return w.path + ": " + w.err.Error() }
func (w *writeError) Unwrap() error { return w.err }

const (
	copyBuf = 1 << 20
	// файлы до этого размера из потоковых архивов (7z, rar, tar) читаются в
	// память и пишутся на диск параллельно, пока распаковывается следующий
	maxQueuedFile = 32 << 20
	// сколько всего данных может ждать записи
	queueBudget = 256 << 20
)

// Extract распаковывает (или проверяет) архив.
//
// Скорость упирается не в распаковку, а в создание файлов: CreateFile с
// антивирусом стоит дороже, чем распаковать небольшой файл. Поэтому файлы
// пишет пул горутин: у zip каждая еще и распаковывает свой файл (файлы zip
// независимы), у потоковых форматов распаковка идет одна, а запись -
// параллельно с ней. Папки создаются один раз (кэш), существующий файл
// ловится самим открытием (O_EXCL), без отдельной проверки.
func Extract(ctx context.Context, a *Archive, opt ExtractOptions, job *Job) (ExtractResult, error) {
	var (
		res ExtractResult
		mu  sync.Mutex // res, dirs, mode и вопросы о конфликтах
	)
	zone := readZone(a.Path) // архив скачан из интернета: пометка переходит на файлы
	sel := map[string]bool{}
	for _, s := range opt.Select {
		sel[s] = true
	}
	selected := func(p string) bool {
		if len(sel) == 0 {
			return true
		}
		for {
			if sel[p] {
				return true
			}
			i := strings.LastIndexByte(p, '/')
			if i < 0 {
				return false
			}
			p = p[:i]
		}
	}

	var total int64
	files := 0
	for _, e := range a.Entries {
		if e.Dir || !selected(e.Path) {
			continue
		}
		files++
		if e.Size < 0 || total < 0 {
			total = -1
			continue
		}
		total += e.Size
	}
	job.Total.Store(total)

	var made sync.Map // папки, которые точно есть
	ensureDir := func(dir string) error {
		if _, ok := made.Load(dir); ok {
			return nil
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return &writeError{dir, err}
		}
		made.Store(dir, true)
		return nil
	}
	if !opt.Test {
		if err := ensureDir(opt.Dest); err != nil {
			return res, err
		}
	}
	type dirTime struct {
		path string
		mod  time.Time
	}
	var dirs []dirTime
	mode := opt.Conflict
	skip := func(s string) {
		mu.Lock()
		res.Skipped = append(res.Skipped, s)
		mu.Unlock()
	}
	count := func(n int64) {
		mu.Lock()
		res.Files++
		res.Bytes += n
		mu.Unlock()
		job.Files.Add(1)
	}

	// create открывает новый файл; ok=false - файл уже есть и его решили не трогать.
	create := func(target string, e Entry) (*os.File, bool, error) {
		const flags = os.O_CREATE | os.O_EXCL | os.O_WRONLY
		out, err := os.OpenFile(target, flags, 0o666)
		if err != nil && !fileExists(filepath.Dir(target)) {
			if err := ensureDir(filepath.Dir(target)); err != nil {
				return nil, false, err
			}
			out, err = os.OpenFile(target, flags, 0o666)
		}
		if err == nil {
			return out, true, nil
		}
		old, serr := os.Lstat(target)
		if serr != nil {
			return nil, false, &writeError{target, err}
		}
		if old.IsDir() {
			skip(e.Path + " - на этом месте папка")
			return nil, false, nil
		}
		mu.Lock()
		c := mode
		if c == ConflictAsk && opt.Ask != nil {
			var all bool
			c, all = opt.Ask(target, e, old)
			if all {
				mode = c
			}
		}
		mu.Unlock()
		switch c {
		case ConflictSkip, ConflictAsk:
			mu.Lock()
			res.Existing++
			mu.Unlock()
			job.Done.Add(max(e.Size, 0))
			return nil, false, nil
		case ConflictCancel:
			return nil, false, context.Canceled
		}
		if old.Mode()&0o200 == 0 { // только для чтения
			_ = os.Chmod(target, 0o666)
		}
		// пишем рядом и подменяем только после успешной записи: при отмене
		// или ошибке старый файл остается целым
		tmp := target + partSuffix
		out, err = os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o666)
		if err != nil {
			return nil, false, &writeError{target, err}
		}
		return out, true, nil
	}

	// writeFile пишет first, затем остаток из rest (может быть nil).
	writeFile := func(ctx context.Context, e Entry, target string, first []byte, rest io.Reader, buf []byte) error {
		out, ok, err := create(target, e)
		if err != nil || !ok {
			return err
		}
		fail := func(err error) error {
			out.Close()
			os.Remove(out.Name()) // новый файл или временный рядом со старым
			return err
		}
		if _, err := out.Write(first); err != nil {
			return fail(&writeError{target, err})
		}
		job.Done.Add(int64(len(first)))
		var written int64
		if rest != nil {
			written, err = copyCtx(ctx, out, rest, buf, job)
			if err != nil {
				var we *writeError
				if errors.As(err, &we) {
					we.path = target
					return fail(we)
				}
				if errors.Is(err, context.Canceled) {
					return fail(err)
				}
				return fail(fmt.Errorf("%s: %w", e.Path, err))
			}
		}
		if !e.Mod.IsZero() {
			setFileTime(out, e.Mod) // по открытому файлу - без лишнего открытия
		}
		if err := out.Close(); err != nil {
			os.Remove(out.Name())
			return &writeError{target, err}
		}
		if out.Name() != target {
			if err := os.Rename(out.Name(), target); err != nil {
				os.Remove(out.Name())
				return &writeError{target, err}
			}
		}
		if zone != nil {
			markZone(target, zone)
		}
		count(int64(len(first)) + written)
		return nil
	}

	// doFile распаковывает (или проверяет) один файл потоком.
	doFile := func(ctx context.Context, e Entry, target string, open opener, buf []byte) error {
		job.setCurrent(e.Path)
		rc, err := open()
		if err != nil {
			return err
		}
		defer rc.Close()
		if opt.Test {
			n, err := copyCtx(ctx, io.Discard, rc, buf, job)
			if err != nil {
				return fmt.Errorf("%s: %w", e.Path, err)
			}
			count(n)
			return nil
		}
		// первый кусок читаем до создания файла: при неверном пароле не останется пустышек
		n, err := io.ReadFull(rc, buf)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return fmt.Errorf("%s: %w", e.Path, err)
		}
		if n < len(buf) { // файл целиком в buf
			return writeFile(ctx, e, target, buf[:n], nil, nil)
		}
		first := append([]byte(nil), buf[:n]...)
		return writeFile(ctx, e, target, first, rc, buf)
	}

	parallelZip := a.isZip && files > 1
	// замеры: создание файла (CreateFile с проверкой антивирусом) в Windows идет
	// почти по очереди; zip быстрее всего на 2-4 потоках, 7z - на 6-8
	workers := min(max(runtime.NumCPU(), 2), 6, max(files, 1))
	if files < 2 || opt.Test && !parallelZip {
		workers = 1
	}

	type work struct {
		e      Entry
		target string
		open   opener // zip: распаковать в горутине
		data   []byte // потоковые форматы: уже распаковано
	}
	var (
		wg       sync.WaitGroup
		queue    chan work
		firstErr error
		errOnce  sync.Once
		budgetMu sync.Mutex
		budget   = sync.NewCond(&budgetMu)
		inFlight int64
	)
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	setErr := func(err error) {
		errOnce.Do(func() { firstErr = err; cancel() })
	}
	release := func(n int64) {
		budgetMu.Lock()
		inFlight -= n
		budgetMu.Unlock()
		budget.Broadcast()
	}
	if workers > 1 {
		queue = make(chan work, workers*2)
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				buf := make([]byte, copyBuf)
				for w := range queue {
					if wctx.Err() == nil {
						var err error
						if w.open != nil {
							err = doFile(wctx, w.e, w.target, w.open, buf)
						} else {
							job.setCurrent(w.e.Path)
							err = writeFile(wctx, w.e, w.target, w.data, nil, nil)
						}
						if err != nil {
							setErr(err)
						}
					}
					if w.data != nil {
						release(int64(len(w.data)))
					}
				}
			}()
		}
	}
	var wait func()
	if workers > 1 {
		wait = func() {
			close(queue)
			wg.Wait()
		}
	}
	send := func(w work) error {
		select {
		case queue <- w:
			return nil
		case <-wctx.Done():
			if w.data != nil {
				release(int64(len(w.data)))
			}
			return wctx.Err()
		}
	}
	buf := make([]byte, copyBuf)

	err := a.walkWait(wctx, &job.Read, func(e Entry, open opener) error {
		if !selected(e.Path) {
			return nil
		}
		rel := e.Path
		if opt.Base != "" {
			rel = strings.TrimPrefix(rel, opt.Base+"/")
		}
		if e.Link {
			skip(e.Path + " - ссылка")
			return nil
		}
		var target string
		if !opt.Test {
			var ok bool
			if target, ok = safeJoin(opt.Dest, rel); !ok {
				skip(e.Path + " - путь выходит за папку распаковки")
				return nil
			}
		}
		if e.Dir {
			if opt.Test {
				return nil
			}
			if err := ensureDir(target); err != nil {
				return err
			}
			mu.Lock()
			dirs = append(dirs, dirTime{target, e.Mod})
			res.Dirs++
			mu.Unlock()
			return nil
		}
		switch {
		case workers > 1 && parallelZip:
			return send(work{e: e, target: target, open: open})
		case workers > 1 && e.Size >= 0 && e.Size <= maxQueuedFile:
			// распаковать в память здесь, записать в фоне
			budgetMu.Lock()
			for inFlight > 0 && inFlight+e.Size > queueBudget && wctx.Err() == nil {
				budget.Wait()
			}
			inFlight += e.Size
			budgetMu.Unlock()
			job.setCurrent(e.Path)
			data, err := readAll(open, e.Size)
			if err != nil {
				release(e.Size)
				return fmt.Errorf("%s: %w", e.Path, err)
			}
			if int64(len(data)) != e.Size { // размер в заголовке соврал
				release(e.Size - int64(len(data)))
			}
			return send(work{e: e, target: target, data: data})
		}
		return doFile(wctx, e, target, open, buf)
	}, wait)
	// разбудить ждущих бюджета после отмены
	budget.Broadcast()
	if firstErr != nil && (err == nil || errors.Is(err, context.Canceled)) {
		err = firstErr
	}
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	// время папок - в конце: запись файлов внутрь его сбивает
	for i := len(dirs) - 1; i >= 0; i-- {
		if !dirs[i].mod.IsZero() {
			_ = os.Chtimes(dirs[i].path, dirs[i].mod, dirs[i].mod)
		}
	}
	return res, cleanErr(err)
}

// readAll читает файл архива целиком (size - размер из заголовка).
func readAll(open opener, size int64) ([]byte, error) {
	rc, err := open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data := make([]byte, size)
	n, err := io.ReadFull(rc, data)
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		return data[:n], nil
	}
	if err != nil {
		return nil, err
	}
	// проверка до конца потока: контрольная сумма сверяется на EOF
	var one [1]byte
	if _, err := rc.Read(one[:]); err != nil && err != io.EOF {
		return nil, err
	}
	return data, nil
}

// setFileTime ставит время изменения через открытый дескриптор.
func setFileTime(f *os.File, t time.Time) {
	ft := windows.NsecToFiletime(t.UnixNano())
	_ = windows.SetFileTime(windows.Handle(f.Fd()), nil, &ft, &ft)
}

// cleanErr убирает обертки библиотек («handling file 3: ...») у известных ошибок.
func cleanErr(err error) error {
	var we *writeError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, errNeedPassword):
		return errNeedPassword
	case errors.Is(err, errBadPassword):
		return errBadPassword
	case errors.As(err, &we):
		return we
	}
	return err
}

func copyCtx(ctx context.Context, w io.Writer, r io.Reader, buf []byte, job *Job) (int64, error) {
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		k, rerr := r.Read(buf)
		if k > 0 {
			if _, err := w.Write(buf[:k]); err != nil {
				return n, &writeError{err: err}
			}
			n += int64(k)
			job.Done.Add(int64(k))
		}
		if rerr == io.EOF {
			return n, nil
		}
		if rerr != nil {
			return n, rerr
		}
	}
}

// ---------- имена и папки ----------

// safeJoin кладет путь из архива внутрь dest. false - путь пытается выйти наружу.
func safeJoin(dest, rel string) (string, bool) {
	parts := []string{dest}
	for _, p := range strings.Split(strings.ReplaceAll(rel, `\`, "/"), "/") {
		switch p {
		case "", ".":
			continue
		case "..":
			return "", false
		}
		parts = append(parts, winSafeName(p))
	}
	if len(parts) == 1 {
		return "", false
	}
	t := filepath.Join(parts...)
	r, err := filepath.Rel(dest, t)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", false
	}
	return t, true
}

var reservedNames = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true}

func init() {
	for i := '1'; i <= '9'; i++ {
		reservedNames["COM"+string(i)] = true
		reservedNames["LPT"+string(i)] = true
	}
}

// winSafeName заменяет то, что Windows не разрешает в имени файла.
func winSafeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 32 || strings.ContainsRune(`<>:"|?*`, r) {
			r = '_'
		}
		b.WriteRune(r)
	}
	n := strings.TrimRight(b.String(), ". ")
	if n == "" {
		return "_"
	}
	stem, _, _ := strings.Cut(n, ".")
	if reservedNames[strings.ToUpper(strings.TrimSpace(stem))] {
		n = "_" + n
	}
	return n
}

// smartDest выбирает, куда распаковать архив рядом с base:
// одна папка (или файл) верхнего уровня - прямо в base, если такого имени там еще нет;
// иначе - в новую папку с именем архива («x», «x (2)»...).
// result - то, что появится (выделить в проводнике).
func smartDest(a *Archive, base string) (dest, result string) {
	if r := a.roots(); len(r) == 1 {
		t := filepath.Join(base, winSafeName(r[0]))
		if !fileExists(t) {
			return base, t
		}
	}
	d := uniquePath(filepath.Join(base, winSafeName(archiveStem(a.Name))))
	return d, d
}

func uniquePath(p string) string {
	if !fileExists(p) {
		return p
	}
	ext := ""
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		ext = filepath.Ext(p)
	}
	stem := strings.TrimSuffix(p, ext)
	for i := 2; ; i++ {
		q := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if !fileExists(q) {
			return q
		}
	}
}

// userError - текст ошибки для окна.
func userError(err error) string {
	var we *writeError
	var ve *volumesError
	switch {
	case errors.As(err, &ve):
		return ve.Error()
	case errors.Is(err, errNeedPassword), errors.Is(err, errBadPassword), errors.Is(err, errNotArchive):
		return err.Error()
	case errors.As(err, &we):
		return "Не удалось записать " + we.path + ":\n" + we.err.Error()
	case errors.Is(err, os.ErrNotExist):
		return "Файл не найден: " + err.Error()
	}
	return err.Error()
}
