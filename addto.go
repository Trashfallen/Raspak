package main

// Добавление файлов и папок в существующий архив.
//
// Архив пишется заново во временный файл рядом и подменяет старый только в
// конце: при отмене или ошибке исходный архив не меняется.
// ZIP: старые записи копируются как есть, без распаковки и пересжатия (в том
// числе зашифрованные). Новые сначала собираются обычной упаковкой во
// временный zip (с паролем архива, если он с паролем) и переносятся так же.
// TAR и сжатые TAR распаковываются потоком и сжимаются заново тем же методом.
// 7z, rar и одиночные сжатые файлы Raspak писать не умеет.

import (
	"archive/tar"
	stdzip "archive/zip"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mholt/archives"
)

// canAdd: nil - в этот архив можно добавлять файлы.
func (a *Archive) canAdd() error {
	if a.volumes {
		return errors.New("многотомный архив изменять нельзя")
	}
	if strings.HasPrefix(strings.ToLower(a.Path), strings.ToLower(tempRoot())) {
		return errors.New("это архив внутри другого архива, изменения не попадут во внешний")
	}
	if a.isZip {
		head := make([]byte, 4)
		if f, err := os.Open(a.Path); err == nil {
			_, _ = io.ReadFull(f, head)
			f.Close()
		}
		if !bytes.HasPrefix(head, []byte("PK\x03\x04")) && !bytes.HasPrefix(head, []byte("PK\x05\x06")) {
			return errors.New("в самораспаковывающийся архив добавлять нельзя")
		}
		return nil
	}
	if _, ok := tarCompression(a.format); ok {
		return nil
	}
	return fmt.Errorf("в %s добавлять нельзя, Raspak дописывает только ZIP и TAR (TAR.GZ, TAR.ZST, TAR.XZ…)", a.Kind)
}

// tarCompression: архив - tar, сжатый или нет (comp == nil).
func tarCompression(f archives.Format) (comp archives.Compression, ok bool) {
	switch v := f.(type) {
	case archives.Tar:
		return nil, true
	case archives.CompressedArchive:
		if _, isTar := v.Extraction.(archives.Tar); !isTar || v.Compression == nil {
			return nil, false
		}
		if _, w := v.Compression.(archives.Compressor); !w {
			return nil, false
		}
		return v.Compression, true
	}
	return nil, false
}

// intoDir кладет собранное в папку архива dir.
func intoDir(items []packItem, dir string) {
	if dir == "" {
		return
	}
	for i := range items {
		items[i].name = dir + "/" + items[i].name
	}
}

// addConflicts - новые файлы, которые в архиве уже есть (папки сливаются молча).
func addConflicts(entries []Entry, items []packItem) []string {
	old := map[string]bool{}
	for _, e := range entries {
		if !e.Dir {
			old[strings.ToLower(e.Path)] = true
		}
	}
	var out []string
	for _, it := range items {
		if !it.dir && old[strings.ToLower(it.name)] {
			out = append(out, it.name)
		}
	}
	return out
}

// withoutNames убирает из собранного указанные файлы.
func withoutNames(items []packItem, names []string) []packItem {
	skip := map[string]bool{}
	for _, n := range names {
		skip[strings.ToLower(n)] = true
	}
	out := items[:0:0]
	for _, it := range items {
		if !skip[strings.ToLower(it.name)] {
			out = append(out, it)
		}
	}
	return out
}

// replacedBy: старая запись уступает новой с тем же путем, а все внутри
// старой папки - новому файлу с ее именем. Папка поверх папки - слияние:
// уходит только сама запись папки, содержимое остается.
func replacedBy(items []packItem) func(path string) bool {
	news := map[string]bool{} // путь -> папка
	for _, it := range items {
		news[strings.ToLower(it.name)] = it.dir
	}
	return func(path string) bool {
		k := strings.ToLower(path)
		if _, ok := news[k]; ok {
			return true
		}
		for d, _ := splitPath(k); d != ""; d, _ = splitPath(d) {
			if dir, ok := news[d]; ok && !dir {
				return true
			}
		}
		return false
	}
}

// AddToArchive дописывает items (имена уже с папкой внутри архива) в архив a.
// Если в zip есть файлы с паролем, новые шифруются тем же паролем: без него
// вернется errNeedPassword, с неверным - errBadPassword.
func AddToArchive(ctx context.Context, a *Archive, items []packItem, job *Job) error {
	if err := a.canAdd(); err != nil {
		return err
	}
	if a.isZip && a.Encrypted {
		if err := a.checkPassword(ctx); err != nil {
			return err
		}
	}
	var total int64
	for _, it := range items {
		total += it.size
	}
	tmp := a.Path + ".raspak-tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return &writeError{tmp, err}
	}
	drop := replacedBy(items)
	if a.isZip {
		job.Total.Store(total + a.Size)
		err = addZip(ctx, a, f, items, drop, job)
	} else {
		var old int64
		for _, e := range a.Entries {
			old += max(e.Size, 0)
		}
		job.Total.Store(total + old)
		err = addTar(ctx, a, f, items, drop, job)
	}
	if cerr := f.Close(); err == nil && cerr != nil {
		err = &writeError{tmp, cerr}
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if zone := readZone(a.Path); zone != nil {
		markZone(tmp, zone) // архив из интернета остается помеченным
	}
	if err := os.Rename(tmp, a.Path); err != nil {
		os.Remove(tmp)
		return &writeError{a.Path, err}
	}
	return nil
}

// checkPassword проверяет пароль на первом зашифрованном файле.
func (a *Archive) checkPassword(ctx context.Context) error {
	if a.Password == "" {
		return errNeedPassword
	}
	errFound := errors.New("found")
	err := a.walk(ctx, nil, func(e Entry, open opener) error {
		if !e.Encrypted || e.Dir {
			return nil
		}
		rc, err := open()
		if err != nil {
			return err
		}
		defer rc.Close()
		if _, err := rc.Read(make([]byte, 1)); err != nil && err != io.EOF {
			return err
		}
		return errFound
	})
	if errors.Is(err, errFound) {
		return nil
	}
	return err
}

func addZip(ctx context.Context, a *Archive, out io.Writer, items []packItem, drop func(string) bool, job *Job) error {
	part := a.Path + ".raspak-new"
	defer os.Remove(part)
	pf, err := os.Create(part)
	if err != nil {
		return &writeError{part, err}
	}
	opt := CompressOptions{Format: ".zip", Level: LevelNormal}
	if a.Encrypted {
		opt.Password = a.Password
		err = writeZipEncrypted(ctx, pf, items, opt, job)
	} else {
		err = writeZip(ctx, pf, items, opt, job)
	}
	if cerr := pf.Close(); err == nil && cerr != nil {
		err = &writeError{part, cerr}
	}
	if err != nil {
		return err
	}

	oldR, err := openZipRaw(a.Path)
	if err != nil {
		return err
	}
	defer oldR.Close()
	newR, err := openZipRaw(part)
	if err != nil {
		return err
	}
	defer newR.Close()

	zw := stdzip.NewWriter(out)
	if err := zw.SetComment(oldR.Comment); err != nil {
		return err
	}
	buf := make([]byte, copyBuf)
	for _, f := range oldR.File {
		name := cleanName(decodeZipName(f.Name, f.Flags))
		if name != "" && drop(name) {
			continue
		}
		job.setCurrent(name)
		if err := copyZipRaw(ctx, zw, f, buf, job); err != nil {
			return err
		}
	}
	quiet := &Job{} // новые уже посчитаны при упаковке
	for _, f := range newR.File {
		if err := copyZipRaw(ctx, zw, f, buf, quiet); err != nil {
			return err
		}
	}
	return zw.Close()
}

func openZipRaw(path string) (*stdzip.ReadCloser, error) {
	r, err := stdzip.OpenReader(path)
	if errors.Is(err, stdzip.ErrInsecurePath) {
		err = nil // имена с «..» переносим как есть, распаковка их и так не пустит наружу
	}
	if err != nil {
		return nil, fmt.Errorf("zip: %w", err)
	}
	return r, nil
}

// copyZipRaw переносит запись как есть: сжатые (и зашифрованные) данные и заголовок.
func copyZipRaw(ctx context.Context, zw *stdzip.Writer, f *stdzip.File, buf []byte, job *Job) error {
	r, err := f.OpenRaw()
	if err != nil {
		return err
	}
	fh := f.FileHeader
	w, err := zw.CreateRaw(&fh)
	if err != nil {
		return err
	}
	_, err = copyCtx(ctx, w, r, buf, job)
	return unwrapWrite(err)
}

func addTar(ctx context.Context, a *Archive, out io.Writer, items []packItem, drop func(string) bool, job *Job) error {
	comp, _ := tarCompression(a.format)
	in, err := os.Open(a.Path)
	if err != nil {
		return err
	}
	defer in.Close()
	var r io.Reader = bufio.NewReaderSize(in, streamBuf)
	var cw io.WriteCloser
	switch c := comp.(type) {
	case nil:
		cw = nopWriteCloser{out}
	case archives.Gz:
		c.Multithreaded = true
		comp = c
		cw, err = tarCompressor(out, ".tar.gz", LevelNormal)
	case archives.Zstd:
		cw, err = tarCompressor(out, ".tar.zst", LevelNormal)
	default:
		cw, err = c.(archives.Compressor).OpenWriter(out)
	}
	if err != nil {
		return err
	}
	if comp != nil {
		rc, err := comp.(archives.Decompressor).OpenReader(r)
		if err != nil {
			return a.mapErr(err)
		}
		defer rc.Close()
		r = rc
	}

	tr := tar.NewReader(r)
	tw := tar.NewWriter(cw)
	buf := make([]byte, copyBuf)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return a.mapErr(err)
		}
		name := cleanName(hdr.Name)
		if name != "" && drop(name) {
			continue
		}
		if hdr.Typeflag == tar.TypeGNUSparse {
			hdr.Typeflag = tar.TypeReg // читатель уже развернул разреженный файл
		}
		job.setCurrent(name)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := copyCtx(ctx, tw, tr, buf, job); err != nil {
			return a.mapErr(unwrapWrite(err))
		}
	}
	if err := writeTarItems(ctx, tw, items, job); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return cw.Close()
}
