package main

// Примерный размер архива для окна «Добавить в архив». Читать и сжимать все
// ради цифры в окне долго, поэтому сжимается выборка: до 64 кусков по 128 КБ,
// разложенных равномерно по объему сжимаемых файлов. Доля сжатия выборки
// переносится на весь объем. Уже сжатое (storedExts) zip кладет как есть,
// а tar сжимает целиком, поэтому для tar у него своя выборка поменьше.

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/klauspost/compress/flate"
	"github.com/klauspost/compress/zstd"
)

const (
	estimateChunk  = 128 << 10
	estimateChunks = 64
)

type packEstimate struct {
	Files int                      `json:"files"`
	Bytes int64                    `json:"bytes"`
	Sizes map[string]map[int]int64 `json:"sizes"` // формат -> уровень -> байт
	ZipPw int64                    `json:"zipPw"` // zip с паролем: уровень не влияет
}

// estimatePack оценивает размер архива для всех форматов и уровней сжатия.
func estimatePack(ctx context.Context, sources []string) (packEstimate, error) {
	est := packEstimate{Sizes: map[string]map[int]int64{}}
	items, total, err := collectFiles(ctx, sources, "")
	if err != nil {
		return est, err
	}
	est.Bytes = total
	var (
		comp, stored           []packItem
		compBytes, storedBytes int64
		zipHead                int64 // заголовки zip: локальный + центральный каталог
	)
	for _, it := range items {
		zipHead += 30 + 46 + 2*int64(len(packEntryName(it)))
		if it.dir {
			continue
		}
		est.Files++
		zipHead += 16 // дескриптор данных
		if it.size == 0 {
			continue
		}
		if storedExts[strings.ToLower(filepath.Ext(it.name))] {
			stored = append(stored, it)
			storedBytes += it.size
		} else {
			comp = append(comp, it)
			compBytes += it.size
		}
	}
	samples, err := estimateSamples(ctx, comp, compBytes, estimateChunks)
	if err != nil {
		return est, err
	}
	storedSamples, err := estimateSamples(ctx, stored, storedBytes, estimateChunks/4)
	if err != nil {
		return est, err
	}
	levels := []int{LevelFast, LevelNormal, LevelMax}
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	// put переносит сжатие выборки на объем; stored < 0 - уже сжатое как есть
	put := func(format string, level int, head, packed, storedPacked int64) {
		size := head + samples.scale(compBytes, packed)
		if storedPacked < 0 {
			size += storedBytes
		} else {
			size += storedSamples.scale(storedBytes, storedPacked)
		}
		mu.Lock()
		if est.Sizes[format] == nil {
			est.Sizes[format] = map[int]int64{}
		}
		est.Sizes[format][level] = size
		mu.Unlock()
	}
	// 7z: на кусках выборки словарь не играет роли, оценка одна на все уровни
	wg.Add(1)
	go func() {
		defer wg.Done()
		head := 32 + 64 + 30*int64(len(items))
		for _, it := range items {
			head += 2 * int64(len(it.name))
		}
		packed, storedPacked := sevenZipPacked(samples), sevenZipPacked(storedSamples)
		for _, level := range append(levels, LevelUltra) {
			put(".7z", level, head, packed, storedPacked)
		}
	}()
	for _, level := range levels {
		wg.Add(3)
		go func() {
			defer wg.Done()
			put(".zip", level, zipHead+22, zipPacked(samples, level), -1)
		}()
		go func() {
			defer wg.Done()
			put(".tar.gz", level, 18+40*int64(len(items)), tarGzPacked(samples, level), tarGzPacked(storedSamples, level))
		}()
		go func() {
			defer wg.Done()
			put(".tar.zst", level, 40*int64(len(items)), tarZstPacked(samples, level), tarZstPacked(storedSamples, level))
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return est, err
	}
	// выигрыш «Ультра» - дальние повторы между файлами, выборка их не видит:
	// для нее оценка - размер «Максимума», то есть верхняя граница
	est.Sizes[".tar.zst"][LevelUltra] = est.Sizes[".tar.zst"][LevelMax]
	// AES-256: соль, проверка пароля и подпись на файл, поле AES в двух заголовках
	est.ZipPw = est.Sizes[".zip"][LevelNormal] + 50*int64(est.Files)
	return est, nil
}

// sampleSet - выборка. solid: файлы целиком и подряд, как в архиве; иначе
// куски из далеких мест, и в tar они сжимаются каждый сам по себе: одним
// потоком zstd нашел бы повторы между ними, которых в архиве не увидит.
type sampleSet struct {
	parts [][]byte
	solid bool
}

// scale: размер total байт, если они сожмутся как выборка.
func (s sampleSet) scale(total, packed int64) int64 {
	var raw int64
	for _, p := range s.parts {
		raw += int64(len(p))
	}
	if raw == 0 {
		return total
	}
	return int64(float64(total) * float64(packed) / float64(raw))
}

// estimateSamples читает выборку: мелкие наборы целиком, иначе chunks кусков
// в равномерно разнесенных по объему точках.
func estimateSamples(ctx context.Context, items []packItem, total int64, chunks int64) (sampleSet, error) {
	var set sampleSet
	read := func(it packItem, off, n int64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		f, err := os.Open(it.disk)
		if err != nil {
			return nil // нет доступа - оценим по остальным
		}
		defer f.Close()
		b := make([]byte, n)
		k, err := f.ReadAt(b, off)
		if err != nil && err != io.EOF {
			return nil
		}
		if k > 0 {
			set.parts = append(set.parts, b[:k])
		}
		return nil
	}
	if total <= estimateChunk*chunks && len(items) <= 4*int(chunks) {
		set.solid = true
		for _, it := range items {
			if err := read(it, 0, it.size); err != nil {
				return set, err
			}
		}
		return set, nil
	}
	step := total / chunks
	i, base := 0, int64(0) // файл и его начало в общем объеме
	lastFile, lastOff := -1, int64(-1)
	for k := range chunks {
		pos := step*k + step/2
		for base+items[i].size <= pos {
			base += items[i].size
			i++
		}
		it := items[i]
		off := min(max(pos-base-estimateChunk/2, 0), max(it.size-estimateChunk, 0))
		if i == lastFile && off == lastOff {
			continue
		}
		lastFile, lastOff = i, off
		if err := read(it, off, min(estimateChunk, it.size)); err != nil {
			return set, err
		}
	}
	return set, nil
}

// zipPacked: каждый файл сжимается отдельно, несжимаемое кладется как есть.
func zipPacked(set sampleSet, level int) int64 {
	var buf bytes.Buffer
	fw, _ := flate.NewWriter(&buf, level)
	var n int64
	for _, s := range set.parts {
		buf.Reset()
		fw.Reset(&buf)
		_, _ = fw.Write(s)
		_ = fw.Close()
		n += int64(min(buf.Len(), len(s)))
	}
	return n
}

type resetWriter interface {
	io.WriteCloser
	Reset(io.Writer)
}

// tarPacked: сплошная выборка - одним потоком, куски - каждый отдельно.
func tarPacked(set sampleSet, cw resetWriter) int64 {
	var cnt countWriter
	cw.Reset(&cnt)
	for _, s := range set.parts {
		_, _ = cw.Write(s)
		if !set.solid {
			_ = cw.Close()
			cw.Reset(&cnt)
		}
	}
	_ = cw.Close()
	return int64(cnt)
}

func tarGzPacked(set sampleSet, level int) int64 {
	fw, _ := flate.NewWriter(io.Discard, level)
	return tarPacked(set, fw)
}

func tarZstPacked(set sampleSet, level int) int64 {
	cw, err := tarCompressor(io.Discard, ".tar.zst", level)
	if err != nil {
		return 0
	}
	return tarPacked(set, cw.(*zstd.Encoder))
}

// sevenZipPacked: куски сжимаются параллельно, каждый со своим словарем.
func sevenZipPacked(set sampleSet) int64 {
	parts := set.parts
	if set.solid && len(parts) > 1 {
		parts = [][]byte{bytes.Join(parts, nil)}
	}
	var (
		n   atomic.Int64
		wg  sync.WaitGroup
		sem = make(chan struct{}, runtime.NumCPU())
	)
	for _, p := range parts {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			c, err := lzma2Encode(p, sevenZipConfig(LevelNormal).dict)
			if err != nil {
				c = p
			}
			n.Add(int64(len(c)))
		}()
	}
	wg.Wait()
	return n.Load()
}

type countWriter int64

func (c *countWriter) Write(p []byte) (int, error) {
	*c += countWriter(len(p))
	return len(p), nil
}
