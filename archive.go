package main

// Чтение архивов: список содержимого и потоковый обход файлов.
// zip - через yeka/zip (умеет пароли ZipCrypto и AES), остальное - через
// mholt/archives: 7z, rar (в том числе многотомные), tar и сжатые tar,
// одиночные сжатые файлы (.gz, .xz, .bz2, .zst, .lz4, .br, .lz, .sz).
// Тома .001, .002... (разбиение 7-Zip и Total Commander) читаются как один файл.

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/klauspost/compress/zstd"
	"github.com/mholt/archives"
	"github.com/nwaples/rardecode/v2"
	"github.com/ulikunitz/xz"
	"github.com/yeka/zip"
	"golang.org/x/text/encoding/charmap"
)

var (
	errNeedPassword = errors.New("архив защищен паролем")
	errBadPassword  = errors.New("неверный пароль или архив поврежден")
	errNotArchive   = errors.New("это не архив или формат не поддерживается")
)

// Entry - файл или папка в архиве.
type Entry struct {
	Path      string // путь через "/", без "/" по краям
	Dir       bool
	Link      bool  // символическая или жесткая ссылка - не распаковывается
	Size      int64 // -1 - неизвестен (одиночный сжатый файл)
	Packed    int64 // -1 - неизвестен
	Mod       time.Time
	Encrypted bool
}

// opener открывает содержимое текущего файла; действует только внутри обхода.
type opener func() (io.ReadCloser, error)

type Archive struct {
	Path      string // как открыли (для томов .001 - первый том)
	Name      string // имя для заголовков, без .001
	Kind      string // ZIP, 7Z, RAR, TAR.GZ, GZ...
	Size      int64  // размер файлов архива на диске
	Password  string
	Entries   []Entry
	Encrypted bool // есть зашифрованные файлы (для 7z узнается только при распаковке)

	single  bool // сжатый файл без архива внутри
	isZip   bool
	volumes bool // тома .001
	format  archives.Format
}

func init() {
	// методы сжатия zip сверх deflate/store, которые встречаются на практике
	zip.RegisterDecompressor(12, func(r io.Reader) io.ReadCloser { return io.NopCloser(bzip2.NewReader(r)) })
	zip.RegisterDecompressor(93, func(r io.Reader) io.ReadCloser {
		d, err := zstd.NewReader(r)
		if err != nil {
			return errReadCloser{err}
		}
		return d.IOReadCloser()
	})
	zip.RegisterDecompressor(95, func(r io.Reader) io.ReadCloser {
		d, err := xz.NewReader(r)
		if err != nil {
			return errReadCloser{err}
		}
		return io.NopCloser(d)
	})
}

type errReadCloser struct{ err error }

func (e errReadCloser) Read([]byte) (int, error) { return 0, e.err }
func (e errReadCloser) Close() error             { return nil }

// OpenArchive определяет формат и читает список файлов.
func OpenArchive(ctx context.Context, path, password string, read *atomic.Int64) (*Archive, error) {
	path = firstVolume(path)
	if err := checkVolumes(path); err != nil {
		return nil, err
	}
	a := &Archive{Path: path, Password: password, Name: filepath.Base(path)}
	src, err := openSource(path, nil)
	if err != nil {
		return nil, err
	}
	a.Size = src.Size()
	a.volumes = len(src.files) > 1 || volumeRe.MatchString(path)
	if a.volumes {
		a.Name = volumeRe.ReplaceAllString(a.Name, "")
	}

	head := make([]byte, 4)
	_, _ = src.ReadAt(head, 0)
	if bytes.HasPrefix(head, []byte("PK\x03\x04")) || bytes.HasPrefix(head, []byte("PK\x05\x06")) {
		a.isZip, a.Kind = true, "ZIP"
	} else {
		format, _, err := archives.Identify(ctx, a.Name, io.NewSectionReader(src, 0, src.Size()))
		if err != nil {
			src.Close()
			if errors.Is(err, archives.NoMatch) {
				return nil, errNotArchive
			}
			return nil, err
		}
		a.format, a.Kind = format, kindOf(format)
		if _, ok := format.(archives.SevenZip); ok {
			encHeader, aes := sevenZipAES(src)
			if encHeader && password == "" {
				src.Close()
				return nil, errNeedPassword
			}
			a.Encrypted = aes
		}
		if _, ok := format.(archives.Zip); ok {
			a.isZip = true
		} else if _, ok := format.(archives.Extractor); !ok {
			if _, ok := format.(archives.Decompressor); !ok {
				src.Close()
				return nil, errNotArchive
			}
			a.single = true
		}
	}
	src.Close()

	err = a.walk(ctx, read, func(e Entry, _ opener) error {
		a.Entries = append(a.Entries, e)
		if e.Encrypted {
			a.Encrypted = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

func kindOf(f archives.Format) string {
	ext := strings.TrimPrefix(f.Extension(), ".")
	if ca, ok := f.(archives.CompressedArchive); ok && ca.Compression != nil && ca.Extraction != nil {
		ext = strings.TrimPrefix(ca.Extraction.(archives.Format).Extension(), ".") + "." +
			strings.TrimPrefix(ca.Compression.(archives.Format).Extension(), ".")
	}
	return strings.ToUpper(ext)
}

// walk обходит файлы архива по порядку. read считает байты, прочитанные из архива.
func (a *Archive) walk(ctx context.Context, read *atomic.Int64, fn func(Entry, opener) error) error {
	return a.walkWait(ctx, read, fn, nil)
}

// walkWait - как walk, но перед закрытием архива ждет wait: opener
// можно отдать другим горутинам (параллельная распаковка zip).
func (a *Archive) walkWait(ctx context.Context, read *atomic.Int64, fn func(Entry, opener) error, wait func()) error {
	src, err := openSource(a.Path, read)
	if err != nil {
		if wait != nil {
			wait()
		}
		return err
	}
	defer func() {
		if wait != nil {
			wait()
		}
		src.Close()
	}()
	switch {
	case a.isZip:
		return a.walkZip(ctx, src, fn)
	case a.single:
		return a.walkSingle(ctx, src, fn)
	}
	return a.walkArchive(ctx, src, fn)
}

func (a *Archive) walkZip(ctx context.Context, src *source, fn func(Entry, opener) error) error {
	zr, err := zip.NewReader(src, src.Size())
	if err != nil {
		return fmt.Errorf("zip: %w", err)
	}
	for _, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := zipName(&f.FileHeader)
		e := Entry{
			Path:      cleanName(name),
			Dir:       strings.HasSuffix(strings.ReplaceAll(name, `\`, "/"), "/") || f.FileInfo().IsDir(),
			Link:      f.Mode()&fs.ModeSymlink != 0,
			Size:      int64(f.UncompressedSize64),
			Packed:    int64(f.CompressedSize64),
			Mod:       zipModTime(&f.FileHeader),
			Encrypted: f.IsEncrypted(),
		}
		if e.Path == "" {
			continue
		}
		if e.Dir {
			e.Size, e.Packed = 0, 0
		}
		f := f
		open := func() (io.ReadCloser, error) {
			if f.IsEncrypted() {
				if a.Password == "" {
					return nil, errNeedPassword
				}
				f.SetPassword(a.Password)
			}
			rc, err := f.Open()
			if err != nil {
				return nil, a.mapErr(err)
			}
			return &mappedReader{rc, a}, nil
		}
		if err := fn(e, open); err != nil {
			return err
		}
	}
	return nil
}

func (a *Archive) walkSingle(ctx context.Context, src *source, fn func(Entry, opener) error) error {
	var mod time.Time
	if fi, err := os.Stat(a.Path); err == nil {
		mod = fi.ModTime()
	}
	e := Entry{Path: singleName(a.Name), Size: -1, Packed: a.Size, Mod: mod}
	format := fastFormat(a.format)
	var rc io.ReadCloser
	defer func() {
		if rc != nil {
			rc.Close()
		}
	}()
	open := func() (io.ReadCloser, error) {
		r, err := format.(archives.Decompressor).OpenReader(bufio.NewReaderSize(io.NewSectionReader(src, 0, src.Size()), streamBuf))
		if err != nil {
			return nil, err
		}
		rc = r
		return io.NopCloser(&mappedReader{r, a}), nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(e, open)
}

func (a *Archive) walkArchive(ctx context.Context, src *source, fn func(Entry, opener) error) error {
	format := fastFormat(a.format)
	var stream io.Reader = io.NewSectionReader(src, 0, src.Size())
	switch f := format.(type) {
	case archives.CompressedArchive, archives.Tar:
		stream = bufio.NewReaderSize(stream, streamBuf) // потоковые форматы: читаем крупно
	case archives.Rar:
		f.Password = a.Password
		if !a.volumes {
			f.Name = a.Path // многотомный rar: следующие тома библиотека найдет сама
		}
		format = f
	case archives.SevenZip:
		f.Password = a.Password
		format = f
	}
	err := format.(archives.Extractor).Extract(ctx, stream, func(ctx context.Context, fi archives.FileInfo) error {
		e := Entry{
			Path:   cleanName(fi.NameInArchive),
			Dir:    fi.IsDir(),
			Link:   fi.LinkTarget != "" || fi.Mode()&fs.ModeSymlink != 0,
			Size:   fi.Size(),
			Packed: -1,
			Mod:    fi.ModTime(),
		}
		if h, ok := fi.Header.(*rardecode.FileHeader); ok {
			e.Encrypted = h.Encrypted
			e.Packed = h.PackedSize
			if h.UnKnownSize {
				e.Size = -1
			}
		}
		if e.Path == "" {
			return nil
		}
		if e.Dir {
			e.Size, e.Packed = 0, 0
		}
		open := func() (io.ReadCloser, error) {
			f, err := fi.Open()
			if err != nil {
				return nil, a.mapErr(err)
			}
			return &mappedReader{f, a}, nil
		}
		return fn(e, open)
	})
	return a.mapErr(err)
}

const streamBuf = 1 << 20

// fastFormat включает многопоточный gzip (pgzip распаковывает с упреждением).
func fastFormat(f archives.Format) archives.Format {
	switch v := f.(type) {
	case archives.Gz:
		v.Multithreaded = true
		return v
	case archives.CompressedArchive:
		if gz, ok := v.Compression.(archives.Gz); ok {
			gz.Multithreaded = true
			v.Compression = gz
		}
		return v
	}
	return f
}

// mapErr переводит ошибки библиотек о паролях в понятные.
func (a *Archive) mapErr(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, errNeedPassword) || errors.Is(err, errBadPassword) {
		return err
	}
	var we *writeError
	if errors.As(err, &we) {
		return err
	}
	msg := err.Error()
	switch {
	case errors.Is(err, rardecode.ErrArchiveEncrypted), errors.Is(err, rardecode.ErrArchivedFileEncrypted),
		strings.Contains(msg, "no password set"):
		if a.Password == "" {
			return errNeedPassword
		}
		return errBadPassword
	case errors.Is(err, rardecode.ErrBadPassword), errors.Is(err, zip.ErrPassword):
		return errBadPassword
	case errors.Is(err, zip.ErrAlgorithm):
		return errors.New("метод сжатия этого файла не поддерживается")
	case errors.Is(err, rardecode.ErrMultiVolume), errors.Is(err, os.ErrNotExist) && a.Kind == "RAR":
		var pe *fs.PathError
		if errors.As(err, &pe) {
			return errors.New("не хватает следующего тома архива: " + filepath.Base(pe.Path))
		}
		return errors.New("не хватает следующего тома архива")
	}
	// с паролем сбой контрольной суммы или распаковки почти всегда значит неверный пароль
	var pe *fs.PathError
	if a.Password != "" && !errors.As(err, &pe) {
		return errBadPassword
	}
	return err
}

// mappedReader переводит ошибки чтения содержимого.
type mappedReader struct {
	r interface {
		io.Reader
		io.Closer
	}
	a *Archive
}

func (m *mappedReader) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	if err != nil && err != io.EOF {
		err = m.a.mapErr(err)
	}
	return n, err
}

func (m *mappedReader) Close() error { return m.r.Close() }

// ---------- имена ----------

// cleanName приводит путь из архива к виду "a/b/c".
func cleanName(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	parts := strings.Split(name, "/")
	out := parts[:0]
	for _, p := range parts {
		if p != "" && p != "." {
			out = append(out, p)
		}
	}
	return strings.Join(out, "/")
}

// zipName: имена без флага UTF-8 из русской Windows записаны в cp866.
func zipName(h *zip.FileHeader) string { return decodeZipName(h.Name, h.Flags) }

func decodeZipName(name string, flags uint16) string {
	if flags&0x800 != 0 || utf8.ValidString(name) {
		return name
	}
	if s, err := charmap.CodePage866.NewDecoder().String(name); err == nil {
		return s
	}
	return name
}

// zipModTime: точное время из поля NTFS или Unix, иначе время DOS (оно местное).
func zipModTime(h *zip.FileHeader) time.Time {
	b := h.Extra
	for len(b) >= 4 {
		tag, size := binary.LittleEndian.Uint16(b), int(binary.LittleEndian.Uint16(b[2:]))
		if 4+size > len(b) {
			break
		}
		d := b[4 : 4+size]
		switch tag {
		case 0x000a: // NTFS: 4 байта резерва, затем атрибуты (тег 1: mtime, atime, ctime)
			if len(d) >= 4+4+8 && binary.LittleEndian.Uint16(d[4:]) == 1 {
				ft := int64(binary.LittleEndian.Uint64(d[8:]))
				return time.Unix(0, (ft-116444736000000000)*100)
			}
		case 0x5455: // extended timestamp
			if len(d) >= 5 && d[0]&1 != 0 {
				return time.Unix(int64(int32(binary.LittleEndian.Uint32(d[1:]))), 0)
			}
		}
		b = b[4+size:]
	}
	if h.ModifiedDate == 0 {
		return time.Time{}
	}
	d, t := h.ModifiedDate, h.ModifiedTime
	return time.Date(int(d>>9+1980), time.Month(d>>5&0xf), int(d&0x1f),
		int(t>>11), int(t>>5&0x3f), int(t&0x1f*2), 0, time.Local)
}

var (
	volumeRe  = regexp.MustCompile(`(?i)\.\d{3}$`)
	rarPartRe = regexp.MustCompile(`(?i)^(.*\.part)(\d+)(\.rar)$`)
	tarExtRe  = regexp.MustCompile(`(?i)\.(tgz|tbz2?|txz|tzst|tlz4|tlz|tsz)$`)
)

// firstVolume: открыли второй том - читаем с первого.
func firstVolume(path string) string {
	if m := rarPartRe.FindStringSubmatch(path); m != nil && strings.TrimLeft(m[2], "0") != "1" {
		first := m[1] + fmt.Sprintf("%0*d", len(m[2]), 1) + m[3]
		if fileExists(first) {
			return first
		}
	}
	if volumeRe.MatchString(path) && !strings.HasSuffix(path, ".001") {
		first := path[:len(path)-3] + "001"
		if fileExists(first) {
			return first
		}
	}
	return path
}

// volumeKey - один ключ на весь набор томов (имя первого тома, без учета
// регистра): «x.part3.rar» и «x.part1.rar» - одно и то же задание.
func volumeKey(path string) string {
	dir, base := filepath.Split(path)
	if m := rarPartRe.FindStringSubmatch(base); m != nil {
		base = m[1] + fmt.Sprintf("%0*d", len(m[2]), 1) + m[3]
	} else if volumeRe.MatchString(base) {
		base = base[:len(base)-3] + "001"
	}
	return strings.ToLower(filepath.Join(dir, base))
}

// uniqueVolumes оставляет по одному файлу на набор томов (порядок сохраняется).
func uniqueVolumes(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		if k := volumeKey(p); !seen[k] {
			seen[k] = true
			out = append(out, p)
		}
	}
	return out
}

// Окончания недокачанных файлов у торрент-клиентов и браузеров.
var downloadingSuffixes = []string{".part", ".crdownload", ".!qb", ".!ut", ".!bt", ".aria2", ".download", ".partial", ".tmp"}

// volumesError: в наборе томов есть пропуски.
type volumesError struct {
	missing     []string
	downloading []string // из missing - лежат недокачанными
}

func (e *volumesError) Error() string {
	s := "Не хватает томов архива: " + strings.Join(e.missing, ", ") + "."
	if len(e.downloading) > 0 {
		s += "\nЕще качаются: " + strings.Join(e.downloading, ", ") + ". Дождитесь конца загрузки."
	}
	return s
}

// checkVolumes проверяет набор «x.partN.rar» или «x.NNN» до распаковки: все
// тома с 1 до самого большого найденного номера должны лежать рядом.
// Последний том, которого еще нет совсем, отсюда не виден - о нем скажет
// распаковка («не хватает следующего тома»).
func checkVolumes(path string) error {
	dir, base := filepath.Split(path)
	var prefix, suffix string
	width := 3
	if m := rarPartRe.FindStringSubmatch(base); m != nil {
		prefix, suffix, width = m[1], m[3], len(m[2])
	} else if volumeRe.MatchString(base) {
		prefix = base[:len(base)-3]
	} else {
		return nil
	}
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	have := map[int]bool{}
	partial := map[int]string{}
	last := 0
	lp, ls := strings.ToLower(prefix), strings.ToLower(suffix)
	for _, e := range entries {
		name := strings.ToLower(e.Name())
		if e.IsDir() || !strings.HasPrefix(name, lp) {
			continue
		}
		rest := name[len(lp):]
		i := 0
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		n, err := strconv.Atoi(rest[:i])
		if i == 0 || err != nil || n == 0 {
			continue
		}
		tail := rest[i:]
		switch {
		case tail == ls:
			have[n] = true
		case strings.HasPrefix(tail, ls) && slices.Contains(downloadingSuffixes, tail[len(ls):]):
			partial[n] = e.Name()
		default:
			continue
		}
		last = max(last, n)
	}
	var ve volumesError
	for n := 1; n <= last; n++ {
		if have[n] {
			continue
		}
		ve.missing = append(ve.missing, prefix+fmt.Sprintf("%0*d", width, n)+suffix)
		if p, ok := partial[n]; ok {
			ve.downloading = append(ve.downloading, p)
		}
	}
	if len(ve.missing) > 0 {
		return &ve
	}
	return nil
}

// archiveStem - имя архива без расширений: «фото.tar.gz» -> «фото», «x.part1.rar» -> «x».
func archiveStem(name string) string {
	n := volumeRe.ReplaceAllString(name, "")
	if m := rarPartRe.FindStringSubmatch(n); m != nil {
		n = strings.TrimSuffix(m[1], ".part")
	} else if tarExtRe.MatchString(n) {
		n = tarExtRe.ReplaceAllString(n, "")
	} else {
		n = strings.TrimSuffix(n, filepath.Ext(n))
		if strings.EqualFold(filepath.Ext(n), ".tar") {
			n = n[:len(n)-4]
		}
	}
	n = strings.TrimRight(n, ". ")
	if n == "" {
		return "archive"
	}
	return n
}

// singleName - имя файла внутри одиночного сжатого: «отчет.txt.gz» -> «отчет.txt».
func singleName(name string) string {
	n := volumeRe.ReplaceAllString(name, "")
	if tarExtRe.MatchString(n) {
		return tarExtRe.ReplaceAllString(n, "") + ".tar"
	}
	s := strings.TrimSuffix(n, filepath.Ext(n))
	if s == "" || s == n {
		return n + ".out"
	}
	return s
}

// roots - имена верхнего уровня архива.
func (a *Archive) roots() []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range a.Entries {
		r, _, _ := strings.Cut(e.Path, "/")
		if r != "" && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// sevenZipAES смотрит заголовок 7z: encHeader - список файлов зашифрован
// (без пароля библиотека выдает невнятную ошибку lzma), aes - в открытом
// заголовке упомянут шифр AES (06 F1 07 01), то есть файлы под паролем.
func sevenZipAES(r io.ReaderAt) (encHeader, aes bool) {
	start := make([]byte, 32)
	if _, err := r.ReadAt(start, 0); err != nil {
		return false, false
	}
	off := int64(binary.LittleEndian.Uint64(start[12:]))
	size := int64(binary.LittleEndian.Uint64(start[20:]))
	if off < 0 || size <= 0 || size > 64<<20 {
		return false, false
	}
	h := make([]byte, size)
	if _, err := r.ReadAt(h, 32+off); err != nil {
		return false, false
	}
	aes = bytes.Contains(h, []byte{0x06, 0xF1, 0x07, 0x01})
	const kEncodedHeader = 0x17
	return h[0] == kEncodedHeader && aes, aes
}

// ---------- источник: файл или цепочка томов ----------

type source struct {
	*io.SectionReader
	files []*os.File
}

func (s *source) Close() error {
	for _, f := range s.files {
		f.Close()
	}
	return nil
}

// openSource открывает архив; для «x.001» - все тома подряд как один файл.
func openSource(path string, read *atomic.Int64) (*source, error) {
	names := []string{path}
	if strings.HasSuffix(path, ".001") {
		base := path[:len(path)-3]
		for i := 2; ; i++ {
			n := base + fmt.Sprintf("%03d", i)
			if !fileExists(n) {
				break
			}
			names = append(names, n)
		}
	}
	s := &source{}
	m := &multiReaderAt{read: read}
	for _, n := range names {
		f, err := os.Open(n)
		if err != nil {
			s.Close()
			return nil, err
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			s.Close()
			return nil, err
		}
		if fi.IsDir() {
			f.Close()
			s.Close()
			return nil, errNotArchive
		}
		s.files = append(s.files, f)
		m.parts = append(m.parts, f)
		m.offs = append(m.offs, m.size)
		m.size += fi.Size()
	}
	s.SectionReader = io.NewSectionReader(m, 0, m.size)
	return s, nil
}

type multiReaderAt struct {
	parts []*os.File
	offs  []int64 // начало каждой части
	size  int64
	read  *atomic.Int64
}

func (m *multiReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for n < len(p) {
		if off >= m.size {
			return n, io.EOF
		}
		i := len(m.offs) - 1
		for i > 0 && m.offs[i] > off {
			i--
		}
		end := m.size
		if i+1 < len(m.offs) {
			end = m.offs[i+1]
		}
		want := min(int64(len(p)-n), end-off)
		k, err := m.parts[i].ReadAt(p[n:n+int(want)], off-m.offs[i])
		n += k
		off += int64(k)
		if m.read != nil {
			m.read.Add(int64(k))
		}
		if err != nil && !(err == io.EOF && int64(k) == want) {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return n, err
		}
	}
	return n, nil
}

func fileExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// formatSize: «1,2 МБ».
func formatSize(n int64) string {
	if n < 0 {
		return ""
	}
	units := []string{"Б", "КБ", "МБ", "ГБ", "ТБ"}
	f, i := float64(n), 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return strconv.FormatInt(n, 10) + " Б"
	}
	s := strconv.FormatFloat(f, 'f', 1, 64)
	if f >= 100 {
		s = strconv.FormatFloat(f, 'f', 0, 64)
	}
	return strings.Replace(s, ".", ",", 1) + " " + units[i]
}
