package main

// Создание архивов: zip (с паролем - AES-256), tar.gz, tar.zst.
//
// zip без пароля собирается параллельно: файлы до 64 МБ сжимаются в памяти
// пулом горутин, а пишутся в архив по порядку уже готовыми (CreateRaw).
// Большие файлы сжимаются потоком в порядке очереди. Уже сжатое (jpg, mp4,
// zip, 7z...) кладется без сжатия: пересжимать его - только тратить время.
// Архив пишется во временный файл рядом и переименовывается в конце: при
// отмене или ошибке на диске не остается обрывка.

import (
	"archive/tar"
	stdzip "archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/flate"
	"github.com/klauspost/compress/zstd"
	"github.com/klauspost/pgzip"
	"github.com/yeka/zip"
)

// Форматы, которые Raspak умеет создавать.
var packFormats = []struct{ Name, Ext string }{
	{"ZIP", ".zip"},
	{"TAR.GZ", ".tar.gz"},
	{"TAR.ZST", ".tar.zst"},
}

const (
	LevelFast   = 1
	LevelNormal = 5
	LevelMax    = 9
)

type CompressOptions struct {
	Out      string // путь архива
	Format   string // ".zip", ".tar.gz", ".tar.zst"
	Level    int    // LevelFast, LevelNormal, LevelMax
	Password string // только zip
}

type CompressResult struct {
	Files int
	Bytes int64 // исходный объем
	Size  int64 // размер архива
}

// packItem - файл или папка для архива.
type packItem struct {
	disk string // путь на диске
	name string // имя в архиве через "/"
	dir  bool
	size int64
	mod  time.Time
}

// collectFiles обходит выбранное: папка «Фото» попадает в архив как «Фото/...».
// Ссылки и точки соединения (junction) не раскрываются.
func collectFiles(ctx context.Context, sources []string, skip string) ([]packItem, int64, error) {
	var items []packItem
	var total int64
	skip = strings.ToLower(filepath.Clean(skip))
	for _, src := range sources {
		src = filepath.Clean(src)
		base := filepath.Dir(src)
		err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if strings.ToLower(p) == skip {
				return nil // сам создаваемый архив
			}
			if d.Type()&fs.ModeSymlink != 0 || d.Type()&fs.ModeIrregular != 0 {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(base, p)
			if err != nil {
				return err
			}
			it := packItem{disk: p, name: filepath.ToSlash(rel), dir: d.IsDir(), mod: fi.ModTime()}
			if !it.dir {
				it.size = fi.Size()
				total += it.size
			}
			items = append(items, it)
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
	}
	return items, total, nil
}

// Compress создает архив из sources.
func Compress(ctx context.Context, sources []string, opt CompressOptions, job *Job) (CompressResult, error) {
	var res CompressResult
	if opt.Level == 0 {
		opt.Level = LevelNormal
	}
	job.setCurrent("Собираю список файлов")
	items, total, err := collectFiles(ctx, sources, opt.Out)
	if err != nil {
		return res, err
	}
	job.Total.Store(total)
	if err := os.MkdirAll(filepath.Dir(opt.Out), 0o755); err != nil {
		return res, &writeError{filepath.Dir(opt.Out), err}
	}
	tmp := opt.Out + ".raspak-tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return res, &writeError{tmp, err}
	}
	fail := func(err error) (CompressResult, error) {
		f.Close()
		os.Remove(tmp)
		return res, err
	}
	switch {
	case opt.Format == ".zip" && opt.Password != "":
		err = writeZipEncrypted(ctx, f, items, opt, job)
	case opt.Format == ".zip":
		err = writeZip(ctx, f, items, opt, job)
	case opt.Format == ".tar.gz" || opt.Format == ".tar.zst":
		err = writeTar(ctx, f, items, opt, job)
	default:
		err = errors.New("неизвестный формат " + opt.Format)
	}
	if err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return res, &writeError{tmp, err}
	}
	if err := os.Rename(tmp, opt.Out); err != nil {
		os.Remove(tmp)
		return res, &writeError{opt.Out, err}
	}
	for _, it := range items {
		if !it.dir {
			res.Files++
		}
	}
	res.Bytes = total
	if fi, err := os.Stat(opt.Out); err == nil {
		res.Size = fi.Size()
	}
	return res, nil
}

// Уже сжатые форматы: кладем как есть.
var storedExts = map[string]bool{}

func init() {
	for _, e := range strings.Fields(".zip .7z .rar .gz .tgz .bz2 .xz .zst .lz4 .br .cab .jar .apk .docx .xlsx .pptx .odt " +
		".jpg .jpeg .png .gif .webp .heic .avif .mp3 .aac .ogg .opus .flac .m4a .mp4 .m4v .mkv .avi .mov .webm .wmv .pdf .woff2") {
		storedExts[e] = true
	}
}

func packEntryName(it packItem) string {
	if it.dir {
		return it.name + "/"
	}
	return it.name
}

// ---------- zip без пароля: параллельное сжатие ----------

const (
	packInMemory = 64 << 20  // файлы до этого размера сжимаются в памяти параллельно
	packBudget   = 512 << 20 // сколько данных может ждать записи
)

type packed struct {
	fh   *stdzip.FileHeader
	data []byte
	err  error
}

func writeZip(ctx context.Context, w io.Writer, items []packItem, opt CompressOptions, job *Job) error {
	zw := stdzip.NewWriter(w)
	level := opt.Level
	zw.RegisterCompressor(stdzip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, level)
	})

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type work struct {
		it   packItem
		done chan packed
	}
	works := make([]*work, len(items))
	for i, it := range items {
		works[i] = &work{it: it, done: make(chan packed, 1)}
	}
	var (
		budgetMu sync.Mutex
		budget   = sync.NewCond(&budgetMu)
		inFlight int64
	)
	release := func(n int64) {
		budgetMu.Lock()
		inFlight -= n
		budgetMu.Unlock()
		budget.Broadcast()
	}
	queue := make(chan *work, runtime.NumCPU()*2)
	var wg sync.WaitGroup
	for range runtime.NumCPU() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buf bytes.Buffer
			fw, _ := flate.NewWriter(&buf, level) // один на поток: создание дорогое (таблицы)
			for wk := range queue {
				wk.done <- packOne(ctx, wk.it, fw, &buf, job)
			}
		}()
	}
	// раздача: маленькие файлы - в пул, по бюджету памяти
	go func() {
		defer close(queue)
		for _, wk := range works {
			if wk.it.dir || wk.it.size > packInMemory {
				continue
			}
			budgetMu.Lock()
			for inFlight > 0 && inFlight+wk.it.size > packBudget && ctx.Err() == nil {
				budget.Wait()
			}
			inFlight += wk.it.size
			budgetMu.Unlock()
			select {
			case queue <- wk:
			case <-ctx.Done():
				return
			}
		}
	}()

	var err error
	for _, wk := range works {
		if err = ctx.Err(); err != nil {
			break
		}
		it := wk.it
		switch {
		case it.dir:
			fh := &stdzip.FileHeader{Name: packEntryName(it), Modified: it.mod, Method: stdzip.Store}
			fh.SetMode(fs.ModeDir | 0o755)
			_, err = zw.CreateHeader(fh)
		case it.size > packInMemory:
			job.setCurrent(it.name)
			err = packStream(ctx, zw, it, job)
		default:
			var p packed
			select {
			case p = <-wk.done:
			case <-ctx.Done():
				p.err = ctx.Err()
			}
			release(it.size)
			if err = p.err; err == nil {
				var fw io.Writer
				if fw, err = zw.CreateRaw(p.fh); err == nil {
					_, err = fw.Write(p.data)
				}
			}
		}
		if err != nil {
			break
		}
	}
	cancel()
	budget.Broadcast()
	wg.Wait() // done буферизованы: пул не застрянет на отправке
	if err != nil {
		return err
	}
	return zw.Close()
}

// packOne читает и сжимает файл в память.
func packOne(ctx context.Context, it packItem, fw *flate.Writer, buf *bytes.Buffer, job *Job) packed {
	if err := ctx.Err(); err != nil {
		return packed{err: err}
	}
	job.setCurrent(it.name)
	raw, err := os.ReadFile(it.disk)
	if err != nil {
		return packed{err: err}
	}
	job.Done.Add(int64(len(raw)))
	fh := &stdzip.FileHeader{
		Name:               packEntryName(it),
		Modified:           it.mod,
		CRC32:              crc32.ChecksumIEEE(raw),
		UncompressedSize64: uint64(len(raw)),
	}
	fh.SetMode(0o644) // имя в UTF-8 стандартный zip помечает сам
	data := raw
	fh.Method = stdzip.Store
	if !storedExts[strings.ToLower(filepath.Ext(it.name))] && len(raw) > 0 {
		buf.Reset()
		fw.Reset(buf)
		_, _ = fw.Write(raw)
		_ = fw.Close()
		if buf.Len() < len(raw) { // сжалось - берем сжатое
			data = append([]byte(nil), buf.Bytes()...)
			fh.Method = stdzip.Deflate
		}
	}
	fh.CompressedSize64 = uint64(len(data))
	return packed{fh: fh, data: data}
}

// packStream - большой файл потоком (сжатие одним потоком, зато без памяти).
func packStream(ctx context.Context, zw *stdzip.Writer, it packItem, job *Job) error {
	in, err := os.Open(it.disk)
	if err != nil {
		return err
	}
	defer in.Close()
	fh := &stdzip.FileHeader{Name: packEntryName(it), Modified: it.mod, Method: stdzip.Deflate}
	if storedExts[strings.ToLower(filepath.Ext(it.name))] {
		fh.Method = stdzip.Store
	}
	fh.SetMode(0o644)
	fw, err := zw.CreateHeader(fh)
	if err != nil {
		return err
	}
	_, err = copyCtx(ctx, fw, in, make([]byte, copyBuf), job)
	return unwrapWrite(err)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// unwrapWrite: copyCtx метит ошибки записи, здесь путь не нужен.
func unwrapWrite(err error) error {
	var we *writeError
	if errors.As(err, &we) && we.path == "" {
		return we.err
	}
	return err
}

// ---------- zip с паролем (AES-256) ----------

func writeZipEncrypted(ctx context.Context, w io.Writer, items []packItem, opt CompressOptions, job *Job) error {
	zw := zip.NewWriter(w)
	buf := make([]byte, copyBuf)
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		fh := &zip.FileHeader{Name: packEntryName(it), Method: zip.Deflate}
		fh.SetModTime(it.mod)
		if !isASCII(fh.Name) {
			fh.Flags |= 0x800 // имя в UTF-8
		}
		if it.dir {
			fh.Method = zip.Store
			if _, err := zw.CreateHeader(fh); err != nil {
				return err
			}
			continue
		}
		if storedExts[strings.ToLower(filepath.Ext(it.name))] {
			fh.Method = zip.Store
		}
		fh.SetPassword(opt.Password)
		fh.SetEncryptionMethod(zip.AES256Encryption)
		fw, err := zw.CreateHeader(fh)
		if err != nil {
			return err
		}
		job.setCurrent(it.name)
		in, err := os.Open(it.disk)
		if err != nil {
			return err
		}
		_, err = copyCtx(ctx, fw, in, buf, job)
		in.Close()
		if err != nil {
			return unwrapWrite(err)
		}
	}
	return zw.Close()
}

// ---------- tar.gz и tar.zst ----------

func writeTar(ctx context.Context, w io.Writer, items []packItem, opt CompressOptions, job *Job) error {
	cw, err := tarCompressor(w, opt.Format, opt.Level)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(cw)
	if err := writeTarItems(ctx, tw, items, job); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return cw.Close()
}

// tarCompressor - сжатие для ".tar.gz" и ".tar.zst"; ".tar" - без сжатия.
func tarCompressor(w io.Writer, format string, level int) (io.WriteCloser, error) {
	switch format {
	case ".tar.gz":
		return pgzip.NewWriterLevel(w, level) // сжатие блоками на всех ядрах
	case ".tar.zst":
		lvl := zstd.SpeedDefault
		switch level {
		case LevelFast:
			lvl = zstd.SpeedFastest
		case LevelMax:
			lvl = zstd.SpeedBestCompression
		}
		return zstd.NewWriter(w, zstd.WithEncoderLevel(lvl), zstd.WithEncoderConcurrency(runtime.NumCPU()))
	case ".tar":
		return nopWriteCloser{w}, nil
	}
	return nil, errors.New("неизвестный формат " + format)
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func writeTarItems(ctx context.Context, tw *tar.Writer, items []packItem, job *Job) error {
	buf := make([]byte, copyBuf)
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr := &tar.Header{Name: it.name, ModTime: it.mod, Mode: 0o644, Typeflag: tar.TypeReg, Size: it.size, Format: tar.FormatPAX}
		if it.dir {
			hdr.Name, hdr.Typeflag, hdr.Mode, hdr.Size = it.name+"/", tar.TypeDir, 0o755, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if it.dir {
			continue
		}
		job.setCurrent(it.name)
		in, err := os.Open(it.disk)
		if err != nil {
			return err
		}
		// файл мог измениться после обхода: tar требует ровно hdr.Size байт
		_, err = copyCtx(ctx, tw, io.LimitReader(in, it.size), buf, job)
		in.Close()
		if err != nil {
			return unwrapWrite(err)
		}
	}
	return nil
}

// ---------- имена ----------

// packName - имя архива для выделенного: один элемент - по нему
// («Фото» -> «Фото.zip», «отчет.docx» -> «отчет.zip»), несколько - по папке.
func packName(sources []string, ext string) string {
	dir := filepath.Dir(sources[0])
	name := filepath.Base(dir)
	if len(sources) == 1 {
		name = filepath.Base(sources[0])
		if fi, err := os.Stat(sources[0]); err == nil && !fi.IsDir() {
			if s := strings.TrimSuffix(name, filepath.Ext(name)); s != "" {
				name = s
			}
		}
	}
	if name == "" || name == "." || strings.HasSuffix(name, `\`) || strings.HasSuffix(name, ":") {
		name = "archive" // корень диска
	}
	name = winSafeName(name)
	p := filepath.Join(dir, name+ext)
	for i := 2; fileExists(p); i++ {
		p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", name, i, ext)) // «x (2).tar.gz», а не «x.tar (2).gz»
	}
	return p
}

// sumText - «12 файлов, 340 МБ -> 120 МБ».
func (r CompressResult) sumText() string {
	return fmt.Sprintf("%s, %s → %s", plural(r.Files, "файл", "файла", "файлов"), formatSize(r.Bytes), formatSize(r.Size))
}
