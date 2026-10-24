package main

// Дерево содержимого архива и модель таблицы для текущей папки.

import (
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/lxn/walk"
)

type node struct {
	name   string
	path   string // полный путь в архиве
	dir    bool
	size   int64 // для папок - сумма содержимого
	packed int64 // -1 - неизвестно
	mod    time.Time
	enc    bool
	files  int // для папок - сколько файлов внутри
}

// tree: содержимое каждой папки архива ("" - корень).
type tree map[string][]*node

func buildTree(entries []Entry) tree {
	t := tree{"": nil}
	nodes := map[string]*node{}
	var ensureDir func(p string) *node
	ensureDir = func(p string) *node {
		if n, ok := nodes[p]; ok {
			return n
		}
		parent, name := splitPath(p)
		if parent != "" {
			ensureDir(parent)
		}
		n := &node{name: name, path: p, dir: true}
		nodes[p] = n
		t[parent] = append(t[parent], n)
		if _, ok := t[p]; !ok {
			t[p] = nil
		}
		return n
	}
	for _, e := range entries {
		if e.Dir {
			n := ensureDir(e.Path)
			n.mod = e.Mod
			continue
		}
		parent, name := splitPath(e.Path)
		if parent != "" {
			ensureDir(parent)
		}
		if old, dup := nodes[e.Path]; dup {
			if old.dir {
				continue
			}
			// tar может дописывать файл повторно - берем последнюю версию
			old.size, old.packed, old.mod, old.enc = e.Size, e.Packed, e.Mod, e.Encrypted
			continue
		}
		n := &node{name: name, path: e.Path, size: e.Size, packed: e.Packed, mod: e.Mod, enc: e.Encrypted}
		nodes[e.Path] = n
		t[parent] = append(t[parent], n)
	}
	// размеры папок
	for p, n := range nodes {
		if n.dir {
			continue
		}
		for d, _ := splitPath(p); d != ""; d, _ = splitPath(d) {
			dn := nodes[d]
			dn.files++
			if n.size > 0 {
				dn.size += n.size
			}
			if n.packed > 0 {
				dn.packed += n.packed
			}
		}
	}
	return t
}

func splitPath(p string) (parent, name string) {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return "", p
	}
	return p[:i], p[i+1:]
}

type listModel struct {
	walk.TableModelBase
	walk.SorterBase
	rows []*node
	dpi  int
}

func (m *listModel) RowCount() int { return len(m.rows) }

func (m *listModel) Value(row, col int) interface{} {
	n := m.rows[row]
	switch col {
	case 0:
		if n.enc {
			return n.name + " *"
		}
		return n.name
	case 1:
		if n.dir && n.files == 0 {
			return ""
		}
		return formatSize(n.size)
	case 2:
		if n.packed <= 0 && n.size > 0 {
			return ""
		}
		return formatSize(n.packed)
	case 3:
		if n.mod.IsZero() {
			return ""
		}
		return n.mod.Local().Format("02.01.2006 15:04")
	}
	return nil
}

func (m *listModel) Image(row int) interface{} {
	n := m.rows[row]
	if ic := fileIcon(n.name, n.dir, m.dpi); ic != nil {
		return ic
	}
	return nil
}

func (m *listModel) setRows(rows []*node) {
	m.rows = append([]*node(nil), rows...)
	m.sortRows()
	m.PublishRowsReset()
}

func (m *listModel) Sort(col int, order walk.SortOrder) error {
	m.SorterBase.Sort(col, order)
	m.sortRows()
	m.PublishRowsReset()
	return nil
}

func (m *listModel) sortRows() {
	col, desc := m.SortedColumn(), m.SortOrder() == walk.SortDescending
	sort.SliceStable(m.rows, func(i, j int) bool {
		a, b := m.rows[i], m.rows[j]
		if a.dir != b.dir {
			return a.dir // папки всегда сверху
		}
		if desc {
			a, b = b, a
		}
		switch col {
		case 1:
			if a.size != b.size {
				return a.size < b.size
			}
		case 2:
			if a.packed != b.packed {
				return a.packed < b.packed
			}
		case 3:
			if !a.mod.Equal(b.mod) {
				return a.mod.Before(b.mod)
			}
		}
		return naturalLess(a.name, b.name)
	})
}

// naturalLess сравнивает без учета регистра и с числами по значению: «2» < «10».
func naturalLess(a, b string) bool {
	ra, rb := []rune(strings.ToLower(a)), []rune(strings.ToLower(b))
	i, j := 0, 0
	for i < len(ra) && j < len(rb) {
		if unicode.IsDigit(ra[i]) && unicode.IsDigit(rb[j]) {
			si := i
			for i < len(ra) && unicode.IsDigit(ra[i]) {
				i++
			}
			sj := j
			for j < len(rb) && unicode.IsDigit(rb[j]) {
				j++
			}
			na := strings.TrimLeft(string(ra[si:i]), "0")
			nb := strings.TrimLeft(string(rb[sj:j]), "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			continue
		}
		if ra[i] != rb[j] {
			return ra[i] < rb[j]
		}
		i++
		j++
	}
	return len(ra)-i < len(rb)-j
}
