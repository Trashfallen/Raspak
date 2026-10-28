package main

// Создание 7z: все файлы одним сплошным (solid) блоком LZMA2, как у 7-Zip.
//
// Сжатие параллельное: поток режется на блоки, каждый блок сжимается
// отдельно и начинается со сброса словаря, а склеенные по порядку блоки -
// обычный поток LZMA2 (так делает и многопоточный 7-Zip). Такие места
// распаковщик Raspak (lzma2par.go) распаковывает тоже параллельно.
// Блок, который почти не сжимается (видео, архивы, шифрованное), кладется
// несжатыми кусками LZMA2: кодировщик LZMA на таких данных медленнее всего.
// Заголовок архива пишется без сжатия, имена через "/", пароля у 7z нет.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"runtime"
	"sync"
	"unicode/utf16"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz/lzma"
)

// sevenZipLevel - настройки LZMA2 для уровня сжатия. Блок мельче - больше
// ядер в работе; сжатие от этого почти не страдает (замер: блок 2 МБ вместо
// сплошного потока - +0,2% к размеру). Поиск BinaryTree у ulikunitz/xz
// сжимает хуже и в 20 раз медленнее HashTable4, его не берем.
type sevenZipLevel struct {
	dict, block int
}

func sevenZipConfig(level int) sevenZipLevel {
	switch level {
	case LevelFast:
		return sevenZipLevel{dict: 1 << 20, block: 4 << 20}
	case LevelMax:
		return sevenZipLevel{dict: 16 << 20, block: 32 << 20}
	case LevelUltra:
		return sevenZipLevel{dict: 64 << 20, block: 128 << 20}
	}
	return sevenZipLevel{dict: 8 << 20, block: 16 << 20}
}

// lzma2DictProp - байт свойства словаря LZMA2 (не меньше dict).
func lzma2DictProp(dict int) byte {
	for p := 0; p < 40; p++ {
		if (2|uint64(p&1))<<(p/2+11) >= uint64(dict) {
			return byte(p)
		}
	}
	return 40
}

// ---------- параллельный LZMA2 ----------

type lzma2Block struct {
	data []byte
	done chan []byte // сжатый блок без конца потока
	err  error
}

// lzma2ParWriter сжимает поток блоками на нескольких ядрах.
type lzma2ParWriter struct {
	cfg   sevenZipLevel
	out   io.Writer
	cur   []byte
	queue chan *lzma2Block // по порядку, к записи
	work  chan *lzma2Block
	wg    sync.WaitGroup
	wrote chan error
}

func newLZMA2ParWriter(out io.Writer, cfg sevenZipLevel) *lzma2ParWriter {
	// память: на поток блок данных и примерно три словаря на сжатие
	workers := max(1, min(runtime.NumCPU(), (1536<<20)/(cfg.block+3*cfg.dict)))
	w := &lzma2ParWriter{
		cfg: cfg, out: out,
		queue: make(chan *lzma2Block, workers),
		work:  make(chan *lzma2Block),
		wrote: make(chan error, 1),
	}
	for range workers {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			for b := range w.work {
				b.done <- w.encode(b)
			}
		}()
	}
	go func() {
		var err error
		for b := range w.queue {
			packed := <-b.done
			if err == nil {
				if err = b.err; err == nil {
					_, err = out.Write(packed)
				}
			}
		}
		w.wrote <- err
	}()
	return w
}

// incompressible: быстрая проба zstd почти ничего не выиграла.
func incompressible(data []byte) bool {
	probe := data[:min(len(data), 4<<20)]
	return len(zstdProbe.EncodeAll(probe, make([]byte, 0, len(probe)))) > len(probe)*97/100
}

var zstdProbe, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(runtime.NumCPU()))

// lzma2Stored - данные несжатыми кусками LZMA2 по 64 КБ, первый со сбросом словаря.
func lzma2Stored(data []byte) []byte {
	out := make([]byte, 0, len(data)+len(data)/65536*3+3)
	ctrl := byte(0x01)
	for len(data) > 0 {
		n := min(len(data), 65536)
		out = append(out, ctrl, byte((n-1)>>8), byte(n-1))
		out = append(out, data[:n]...)
		data = data[n:]
		ctrl = 0x02
	}
	return out
}

func (w *lzma2ParWriter) encode(b *lzma2Block) []byte {
	p, err := lzma2Encode(b.data, w.cfg.dict)
	b.data, b.err = nil, err
	return p
}

// lzma2Encode сжимает блок в куски LZMA2 со сбросом словаря в начале, без
// конца потока: блоки можно склеивать.
func lzma2Encode(data []byte, dict int) ([]byte, error) {
	if incompressible(data) {
		return lzma2Stored(data), nil
	}
	var buf bytes.Buffer
	enc, err := lzma.Writer2Config{DictCap: dict}.NewWriter2(&buf)
	if err == nil {
		if _, err = enc.Write(data); err == nil {
			err = enc.Close()
		}
	}
	p := buf.Bytes()
	if err == nil && (len(p) == 0 || p[len(p)-1] != 0) {
		err = errors.New("lzma2: нет конца потока")
	}
	if err != nil {
		return nil, err
	}
	return p[:len(p)-1], nil
}

func (w *lzma2ParWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		if w.cur == nil {
			w.cur = make([]byte, 0, w.cfg.block)
		}
		k := min(len(p), w.cfg.block-len(w.cur))
		w.cur = append(w.cur, p[:k]...)
		p = p[k:]
		if len(w.cur) == w.cfg.block {
			w.flush()
		}
	}
	return n, nil
}

func (w *lzma2ParWriter) flush() {
	if len(w.cur) == 0 {
		return
	}
	b := &lzma2Block{data: w.cur, done: make(chan []byte, 1)}
	w.cur = nil
	w.queue <- b // ждет, если впереди уже столько блоков, сколько потоков
	w.work <- b
}

// Close дожимает последний блок и пишет конец потока.
func (w *lzma2ParWriter) Close() error {
	w.flush()
	close(w.work)
	close(w.queue)
	w.wg.Wait()
	if err := <-w.wrote; err != nil {
		return err
	}
	_, err := w.out.Write([]byte{0})
	return err
}

// abort останавливает потоки без записи (отмена или ошибка чтения).
func (w *lzma2ParWriter) abort() {
	w.cur = nil
	close(w.work)
	close(w.queue)
	w.wg.Wait()
	<-w.wrote
}

// ---------- архив ----------

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func write7z(ctx context.Context, f *os.File, items []packItem, opt CompressOptions, job *Job) error {
	cfg := sevenZipConfig(opt.Level)
	if _, err := f.Write(make([]byte, 32)); err != nil { // место под начальный заголовок
		return err
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	packed := &countingWriter{w: bw}
	var (
		sizes  []int64 // непустые файлы по порядку
		crcs   []uint32
		total  int64
		enc    *lzma2ParWriter
		buf    = make([]byte, copyBuf)
		filled = make([]bool, len(items)) // в потоке есть данные
	)
	for i, it := range items {
		if err := ctx.Err(); err != nil {
			if enc != nil {
				enc.abort()
			}
			return err
		}
		if it.dir || it.size == 0 {
			continue
		}
		if enc == nil {
			enc = newLZMA2ParWriter(packed, cfg)
		}
		job.setCurrent(it.name)
		in, err := os.Open(it.disk)
		if err != nil {
			enc.abort()
			return err
		}
		h := crc32.NewIEEE()
		n, err := copyCtx(ctx, io.MultiWriter(enc, h), io.LimitReader(in, it.size), buf, job)
		in.Close()
		if err != nil {
			enc.abort()
			return unwrapWrite(err)
		}
		if n > 0 {
			filled[i] = true
			sizes = append(sizes, n)
			crcs = append(crcs, h.Sum32())
			total += n
		}
	}
	if enc != nil {
		if err := enc.Close(); err != nil {
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	hdr := sevenZipHeader(items, filled, sizes, crcs, total, packed.n, cfg)
	if _, err := f.Write(hdr); err != nil {
		return err
	}
	start := make([]byte, 32)
	copy(start, "7z\xBC\xAF\x27\x1C\x00\x04")
	binary.LittleEndian.PutUint64(start[12:], uint64(packed.n))
	binary.LittleEndian.PutUint64(start[20:], uint64(len(hdr)))
	binary.LittleEndian.PutUint32(start[28:], crc32.ChecksumIEEE(hdr))
	binary.LittleEndian.PutUint32(start[8:], crc32.ChecksumIEEE(start[12:]))
	_, err := f.WriteAt(start, 0)
	return err
}

// sevenZipHeader - заголовок 7z без сжатия.
func sevenZipHeader(items []packItem, filled []bool, sizes []int64, crcs []uint32, total, packSize int64, cfg sevenZipLevel) []byte {
	var h sevenZipBuf
	h.byte(0x01) // kHeader
	if len(sizes) > 0 {
		h.byte(0x04)                               // kMainStreamsInfo
		h.byte(0x06).num(0).num(1)                 // kPackInfo: с начала, один поток
		h.byte(0x09).num(uint64(packSize)).byte(0) // kSize, kEnd
		h.byte(0x07).byte(0x0B).num(1).byte(0)     // kUnPackInfo, kFolder: одна папка, не внешняя
		h.num(1).byte(0x21).byte(0x21)             // один кодер: id из 1 байта + свойства; LZMA2
		h.num(1).byte(lzma2DictProp(cfg.dict))
		h.byte(0x0C).num(uint64(total)).byte(0) // kCodersUnPackSize, kEnd
		h.byte(0x08)                            // kSubStreamsInfo
		if len(sizes) > 1 {
			h.byte(0x0D).num(uint64(len(sizes)))
			h.byte(0x09)
			for _, s := range sizes[:len(sizes)-1] {
				h.num(uint64(s))
			}
		}
		h.byte(0x0A).byte(1) // kCRC, все есть
		for _, c := range crcs {
			h.u32(c)
		}
		h.byte(0).byte(0) // конец SubStreamsInfo и MainStreamsInfo
	}

	h.byte(0x05).num(uint64(len(items))) // kFilesInfo
	var empty, emptyFile []bool
	anyEmpty, anyEmptyFile := false, false
	for i, it := range items {
		e := !filled[i]
		empty = append(empty, e)
		if e {
			anyEmpty = true
			emptyFile = append(emptyFile, !it.dir)
			anyEmptyFile = anyEmptyFile || !it.dir
		}
	}
	if anyEmpty {
		h.prop(0x0E, bitVector(empty)) // kEmptyStream
		if anyEmptyFile {
			h.prop(0x0F, bitVector(emptyFile)) // kEmptyFile
		}
	}
	var names sevenZipBuf
	names.byte(0) // не внешние
	for _, it := range items {
		for _, c := range utf16.Encode([]rune(it.name)) {
			names.u16(c)
		}
		names.u16(0)
	}
	h.prop(0x11, names.Bytes()) // kName
	var times, attrs sevenZipBuf
	times.byte(1).byte(0) // все есть, не внешние
	attrs.byte(1).byte(0)
	for _, it := range items {
		times.u64(uint64(it.mod.UnixNano()/100 + 116444736000000000)) // FILETIME
		if it.dir {
			attrs.u32(0x10) // FILE_ATTRIBUTE_DIRECTORY
		} else {
			attrs.u32(0x20) // FILE_ATTRIBUTE_ARCHIVE
		}
	}
	h.prop(0x14, times.Bytes()) // kMTime
	h.prop(0x15, attrs.Bytes()) // kWinAttributes
	h.byte(0)                   // конец FilesInfo
	h.byte(0)                   // конец Header
	return h.Bytes()
}

type sevenZipBuf struct{ bytes.Buffer }

func (b *sevenZipBuf) byte(c byte) *sevenZipBuf { b.WriteByte(c); return b }

// num - число 7z: старшие единичные биты первого байта - сколько байт дальше.
func (b *sevenZipBuf) num(v uint64) *sevenZipBuf {
	first, mask, i := byte(0), byte(0x80), 0
	for ; i < 8; i++ {
		if v < 1<<(7*(i+1)) {
			first |= byte(v >> (8 * i))
			break
		}
		first |= mask
		mask >>= 1
	}
	b.WriteByte(first)
	for ; i > 0; i-- {
		b.WriteByte(byte(v))
		v >>= 8
	}
	return b
}

func (b *sevenZipBuf) u16(v uint16) { b.Write(binary.LittleEndian.AppendUint16(nil, v)) }
func (b *sevenZipBuf) u32(v uint32) { b.Write(binary.LittleEndian.AppendUint32(nil, v)) }
func (b *sevenZipBuf) u64(v uint64) { b.Write(binary.LittleEndian.AppendUint64(nil, v)) }

// prop - свойство списка файлов: id, размер, данные.
func (b *sevenZipBuf) prop(id byte, data []byte) {
	b.byte(id).num(uint64(len(data)))
	b.Write(data)
}

// bitVector: старший бит первого байта - первый элемент.
func bitVector(v []bool) []byte {
	out := make([]byte, (len(v)+7)/8)
	for i, on := range v {
		if on {
			out[i/8] |= 0x80 >> (i % 8)
		}
	}
	return out
}
