package main

// Параллельная распаковка LZMA2 для 7z.
//
// LZMA2 - цепочка кусков (chunk) с заголовками. Многопоточный 7-Zip режет
// поток на блоки, каждый из которых начинается со сброса словаря и свойств:
// такой блок распаковывается независимо от предыдущих. Здесь один поток
// быстро читает сжатые данные и режет их по таким местам (это просто
// копирование), блоки распаковываются параллельно, а наружу данные идут строго
// по порядку - и сразу, по мере распаковки текущего блока, а не после него
// целиком: запись файлов начинается без задержки, как у 7-Zip.
// Память ограничена бюджетом. Если сбросов нет (архив сжат в один поток),
// остаток распаковывается обычным потоковым декодером без накопления.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/bodgit/sevenzip"
	"github.com/ulikunitz/xz/lzma"
)

func init() {
	// заменяет штатный однопоточный декодер LZMA2 библиотеки 7z
	sevenzip.RegisterDecompressor([]byte{0x21}, newParallelLZMA2)
}

const (
	lzma2InFlight = 768 << 20 // распакованного, ждущего выдачи
	lzma2Step     = 1 << 20   // столько распаковывается между уведомлениями читателя
)

// lzma2SegMax: блок крупнее - дальше распаковываем потоком (переменная - для тестов).
var lzma2SegMax = 256 << 20

var lzma2Segments atomic.Int64 // для замеров: сколько блоков распаковано параллельно

// lzma2Seg - независимый блок. Распаковщик заполняет data, читатель забирает
// готовую часть [0, avail), не дожидаясь конца блока.
type lzma2Seg struct {
	packed []byte    // куски блока + конец потока (0x00)
	size   int       // распакованный размер
	stream io.Reader // не nil - потоковый остаток вместо блока

	mu    sync.Mutex
	cond  *sync.Cond
	data  []byte
	avail int
	done  bool
	err   error
	pos   int // сколько уже отдано читателю
}

func newSeg() *lzma2Seg {
	s := &lzma2Seg{}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *lzma2Seg) finish(err error) {
	s.mu.Lock()
	s.done, s.err = true, err
	s.mu.Unlock()
	s.cond.Broadcast()
}

type parallelLZMA2 struct {
	src     io.ReadCloser
	dictCap int
	order   chan *lzma2Seg // блоки в порядке выдачи
	work    chan *lzma2Seg // блоки на распаковку

	mu       sync.Mutex
	cond     *sync.Cond
	inFlight int
	closed   bool

	cur    *lzma2Seg
	stream io.Reader
	err    error
}

func newParallelLZMA2(p []byte, _ uint64, readers []io.ReadCloser) (io.ReadCloser, error) {
	if len(readers) != 1 {
		return nil, errors.New("lzma2: need exactly one reader")
	}
	if len(p) != 1 {
		return nil, errors.New("lzma2: not enough properties")
	}
	z := &parallelLZMA2{
		src:     readers[0],
		dictCap: (2 | (int(p[0]) & 1)) << (p[0]/2 + 11), // как в Lzma2Dec.c
		order:   make(chan *lzma2Seg, runtime.NumCPU()*2),
		work:    make(chan *lzma2Seg, runtime.NumCPU()*2),
	}
	cfg := lzma.Reader2Config{DictCap: z.dictCap}
	if err := cfg.Verify(); err != nil {
		return nil, fmt.Errorf("lzma2: %w", err)
	}
	z.cond = sync.NewCond(&z.mu)
	for range runtime.NumCPU() {
		go z.worker()
	}
	go z.split(bufio.NewReaderSize(readers[0], 1<<20))
	return z, nil
}

func (z *parallelLZMA2) worker() {
	for s := range z.work {
		s.finish(z.decode(s))
	}
}

func (z *parallelLZMA2) decode(s *lzma2Seg) error {
	defer func() { s.packed = nil }()
	cfg := lzma.Reader2Config{DictCap: z.dictCap}
	lr, err := cfg.NewReader2(bytes.NewReader(s.packed))
	if err != nil {
		return fmt.Errorf("lzma2: %w", err)
	}
	data := make([]byte, s.size)
	s.mu.Lock()
	s.data = data
	s.mu.Unlock()
	for off := 0; off < s.size; {
		n, err := io.ReadFull(lr, data[off:min(off+lzma2Step, s.size)])
		off += n
		s.mu.Lock()
		s.avail = off
		closed := s.done
		s.mu.Unlock()
		s.cond.Broadcast()
		if err != nil {
			return fmt.Errorf("lzma2: %w", err)
		}
		if closed {
			return nil
		}
	}
	var one [1]byte
	if n, e := lr.Read(one[:]); n != 0 || e != io.EOF {
		return errors.New("lzma2: размер блока не сходится с заголовками")
	}
	return nil
}

// split режет поток на блоки по сбросам словаря.
func (z *parallelLZMA2) split(br *bufio.Reader) {
	defer close(z.order)
	defer close(z.work)
	var seg *lzma2Seg
	fail := func(err error) {
		s := newSeg()
		s.finish(err)
		z.order <- s
	}
	// dispatch отдает готовый блок, дождавшись места в бюджете памяти
	dispatch := func() bool {
		if seg == nil {
			return true
		}
		seg.packed = append(seg.packed, 0x00)
		z.mu.Lock()
		for z.inFlight > 0 && z.inFlight+seg.size > lzma2InFlight && !z.closed {
			z.cond.Wait()
		}
		closed := z.closed
		z.inFlight += seg.size
		z.mu.Unlock()
		if closed {
			return false
		}
		lzma2Segments.Add(1)
		z.order <- seg
		z.work <- seg
		seg = nil
		return true
	}
	hdr := make([]byte, 6)
	for {
		c, err := br.ReadByte()
		if err != nil {
			fail(fmt.Errorf("lzma2: обрыв потока: %w", err))
			return
		}
		if c == 0x00 { // конец потока
			dispatch()
			return
		}
		var hlen, packed, unpacked int
		var reset bool
		switch {
		case c == 0x01 || c == 0x02: // несжатый кусок (0x01 - со сбросом словаря)
			hlen = 3
			if _, err := io.ReadFull(br, hdr[1:3]); err != nil {
				fail(fmt.Errorf("lzma2: обрыв потока: %w", err))
				return
			}
			unpacked = (int(hdr[1])<<8 | int(hdr[2])) + 1
			// режем только по сжатым кускам со сбросом словаря и свойствами:
			// после несжатого 0x01 следующий кусок может опираться на прежние свойства
			packed, reset = unpacked, c == 0x01 && seg == nil
		case c >= 0x80: // сжатый кусок
			hlen = 5
			if c >= 0xC0 { // со свойствами
				hlen = 6
			}
			if _, err := io.ReadFull(br, hdr[1:hlen]); err != nil {
				fail(fmt.Errorf("lzma2: обрыв потока: %w", err))
				return
			}
			unpacked = (int(c&0x1F)<<16 | int(hdr[1])<<8 | int(hdr[2])) + 1
			packed = (int(hdr[3])<<8 | int(hdr[4])) + 1
			reset = c >= 0xE0
		default:
			fail(fmt.Errorf("lzma2: неверный заголовок куска %#x", c))
			return
		}
		hdr[0] = c
		if reset {
			if !dispatch() {
				return
			}
			seg = newSeg()
		} else if seg == nil {
			fail(errors.New("lzma2: поток начинается без сброса словаря"))
			return
		}
		seg.packed = append(seg.packed, hdr[:hlen]...)
		n := len(seg.packed)
		seg.packed = append(seg.packed, make([]byte, packed)...)
		if _, err := io.ReadFull(br, seg.packed[n:]); err != nil {
			fail(fmt.Errorf("lzma2: обрыв потока: %w", err))
			return
		}
		seg.size += unpacked
		if seg.size > lzma2SegMax {
			// сбросов словаря нет - дальше обычным потоком, без накопления
			s := newSeg()
			s.stream = io.MultiReader(bytes.NewReader(seg.packed), br)
			seg = nil
			z.order <- s
			return
		}
	}
}

func (z *parallelLZMA2) Read(p []byte) (int, error) {
	for {
		if z.err != nil {
			return 0, z.err
		}
		if z.stream != nil {
			n, err := z.stream.Read(p)
			if err != nil && err != io.EOF {
				err = fmt.Errorf("lzma2: %w", err)
			}
			return n, err
		}
		if s := z.cur; s != nil {
			s.mu.Lock()
			for s.pos >= s.avail && !s.done {
				s.cond.Wait()
			}
			if s.pos < s.avail {
				n := copy(p, s.data[s.pos:s.avail])
				s.pos += n
				s.mu.Unlock()
				return n, nil
			}
			err := s.err
			s.data = nil
			s.mu.Unlock()
			z.release(s.size) // блок выдан целиком
			z.cur = nil
			if err != nil {
				z.err = err
			}
			continue
		}
		s, ok := <-z.order
		if !ok {
			return 0, io.EOF
		}
		if s.stream != nil {
			cfg := lzma.Reader2Config{DictCap: z.dictCap}
			lr, err := cfg.NewReader2(s.stream)
			if err != nil {
				z.err = fmt.Errorf("lzma2: %w", err)
				continue
			}
			z.stream = lr
			continue
		}
		z.cur = s
	}
}

func (z *parallelLZMA2) release(n int) {
	z.mu.Lock()
	z.inFlight -= n
	z.mu.Unlock()
	z.cond.Broadcast()
}

func (z *parallelLZMA2) Close() error {
	z.mu.Lock()
	z.closed = true
	z.mu.Unlock()
	z.cond.Broadcast()
	err := z.src.Close()
	// дочитать очередь, чтобы разделитель завершился; распаковщики дорабатывают сами
	go func() {
		for s := range z.order {
			s.mu.Lock()
			s.done = true // остановить распаковку блока на следующем шаге
			s.mu.Unlock()
		}
	}()
	return err
}
