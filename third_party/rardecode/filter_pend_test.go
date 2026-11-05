package rardecode

import "testing"

// Фильтр, поставленный в очередь, пока bufBytes собирает блок предыдущего
// фильтра (decoder дописал окно, позиция чтения уже сдвинута), считается от
// начала того блока. Без d.pend смещение уезжало назад и архивы WinRAR 7
// падали с «invalid filter».
func TestQueueFilterWhileCollectingBlock(t *testing.T) {
	d := &decodeReader{size: 1 << 20, win: make([]byte, 1<<20)}
	head := &filterBlock{offset: 0, length: 49152}
	d.fl = []*filterBlock{head}
	// из блока уже взято 47835 байт, окно перешло на начало: r=0, w=17701
	d.r, d.w, d.pend = 0, 17701, 47835
	f := &filterBlock{offset: 0, length: 65536}
	if err := d.queueFilter(f); err != nil {
		t.Fatal(err)
	}
	if want := 47835 + 17701; f.offset != want {
		t.Fatalf("offset = %d, want %d", f.offset, want)
	}
}
