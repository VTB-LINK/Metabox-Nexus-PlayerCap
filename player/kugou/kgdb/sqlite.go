package kgdb

// 最小只读 SQLite 读取器（设计 §6.10）：经 Pager 按需取页，只解析 sqlite_master 与表 b-tree，
// 不持有整库。格式依据 SQLite 文件格式文档 https://www.sqlite.org/fileformat2.html 。

import (
	"bytes"
	"encoding/binary"
	"math"
	"slices"
	"strconv"
	"strings"

	"Metabox-Nexus-PlayerCap/i18n"
)

// 读取器的上限（设计 §6.10）。
const (
	maxDepth       = 32      // b-tree 从根到叶的页数
	maxRecordBytes = 4 << 20 // 单条记录的载荷

	// Open 从 sqlite_master 保留的元数据：表数，以及表名与建表文本的字节数合计。依据：本机真实的
	// KGMusicV3.db 有 140 张表，表数上限取其约 30 倍；文本上限与单条记录同量级。只防畸形库让元数据内存
	// 随 schema 无界增长，超出按整库不支持处理。
	maxTables      = 4096
	maxSchemaBytes = 4 << 20
)

var (
	// ErrCorrupt 表示库的结构异常；具体原因以 %w 包装在外。
	ErrCorrupt = i18n.Sentinel("KuGou database structure is invalid")
	// ErrNoTable 表示 sqlite_master 里没有该表。
	ErrNoTable = i18n.Sentinel("table not found in KuGou database")
	// ErrNoColumn 表示表定义里没有该列。
	ErrNoColumn = i18n.Sentinel("column not found in KuGou database table")
	// ErrUnsupportedTable 表示表的种类不支持（WITHOUT ROWID 表、虚拟表）。只关乎这一张表：
	// 调用方跳过它即可，不据此判整库不支持。
	ErrUnsupportedTable = i18n.Sentinel("KuGou database table kind is not supported")
)

// Kind 是值的存储类型。
type Kind uint8

const (
	Null Kind = iota
	Integer
	Float
	Text
	Blob
)

// Value 是一列的值。Bytes 是独立副本，不与读取器的缓冲区共享。
type Value struct {
	Kind  Kind
	Int   int64
	Float float64
	Bytes []byte
}

// String 返回 Text、Blob 的字节、Integer 与 Float 的十进制文本；Null 为空串。
func (v Value) String() string {
	switch v.Kind {
	case Integer:
		return strconv.FormatInt(v.Int, 10)
	case Float:
		return strconv.FormatFloat(v.Float, 'g', -1, 64)
	case Text, Blob:
		return string(v.Bytes)
	}
	return ""
}

// Int64 返回 Integer 的值，其余类型为 0。
func (v Value) Int64() int64 {
	if v.Kind == Integer {
		return v.Int
	}
	return 0
}

// DB 是只读打开的库：只持有取页器与 sqlite_master 里登记的表，页在 Scan 时按需读取。
type DB struct {
	p      Pager
	tables []table
}

type table struct {
	name string
	root int64
	sql  string
}

// Open 校验文件头并读入 sqlite_master。头部不合要求（非 SQLite、页大小不是 PageSize、读写版本不都为 1
// 即 WAL、每页保留字节不为 0、文本编码不是 UTF-8）或保留的表超出 maxTables、maxSchemaBytes，
// 返回包装 ErrUnsupportedDB 的错误；没有页或 sqlite_master 结构异常返回包装 ErrCorrupt 的错误；取页失败原样返回。
func Open(p Pager) (*DB, error) {
	if p.PageCount() == 0 {
		return nil, i18n.Errorf("%w: database has no pages", ErrCorrupt)
	}
	h, err := p.Page(1)
	if err != nil {
		return nil, err
	}
	switch {
	case !bytes.Equal(h[:16], sqliteMagic):
		return nil, i18n.Errorf("%w: not an SQLite database", ErrUnsupportedDB)
	case binary.BigEndian.Uint16(h[16:]) != PageSize:
		return nil, i18n.Errorf("%w: page size %d, want %d", ErrUnsupportedDB, binary.BigEndian.Uint16(h[16:]), PageSize)
	case h[18] != 1 || h[19] != 1:
		return nil, i18n.Errorf("%w: file format version %d/%d, want 1/1", ErrUnsupportedDB, h[18], h[19])
	case h[20] != 0:
		return nil, i18n.Errorf("%w: %d reserved bytes per page, want 0", ErrUnsupportedDB, h[20])
	case binary.BigEndian.Uint32(h[56:]) != 1:
		return nil, i18n.Errorf("%w: text encoding %d, want 1 (UTF-8)", ErrUnsupportedDB, binary.BigEndian.Uint32(h[56:]))
	}

	db := &DB{p: p}
	w := walker{p: p}
	kept := 0 // 已保留的表名与建表文本字节数
	// sqlite_master 的列：type, name, tbl_name, rootpage, sql。
	err = w.walk(1, func(rowid int64, payload []byte) (bool, error) {
		row, err := decodeRecord(payload, rowid, []int{0, 1, 3, 4})
		if err != nil {
			return false, err
		}
		if row[0].String() != "table" {
			return true, nil
		}
		if len(db.tables) == maxTables {
			return false, i18n.Errorf("%w: schema too large: more than %d tables", ErrUnsupportedDB, maxTables)
		}
		if kept += len(row[1].Bytes) + len(row[3].Bytes); kept > maxSchemaBytes {
			return false, i18n.Errorf("%w: schema too large: names and SQL exceed %d bytes", ErrUnsupportedDB, maxSchemaBytes)
		}
		db.tables = append(db.tables, table{name: row[1].String(), root: row[2].Int64(), sql: row[3].String()})
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return db, nil
}

// Tables 按 sqlite_master 的顺序返回 type='table' 的表名。
func (db *DB) Tables() []string {
	names := make([]string, len(db.tables))
	for i, t := range db.tables {
		names[i] = t.name
	}
	return names
}

// Columns 按建表语句返回表的列名（已去引号）。表不存在、表的种类不支持、建表语句解析不了，
// 分别返回包装 ErrNoTable、ErrUnsupportedTable、ErrCorrupt 的错误（见 lookup）。
func (db *DB) Columns(name string) ([]string, error) {
	_, def, err := db.lookup(name)
	if err != nil {
		return nil, err
	}
	cols := make([]string, len(def.cols))
	for i, c := range def.cols {
		cols[i] = c.name
	}
	return cols, nil
}

// Scan 按 rowid 升序遍历表，每行以 cols 对应的值调用 fn，fn 返回 false 即停。
// 表名、列名不分大小写；INTEGER PRIMARY KEY 列取 rowid；记录的列数少于表定义时缺的列为 Null。
// 表不存在、表的种类不支持、列不存在分别返回包装 ErrNoTable、ErrUnsupportedTable、ErrNoColumn 的错误，
// 均不调用 fn；结构异常返回包装 ErrCorrupt 的错误；取页失败原样返回。
func (db *DB) Scan(name string, cols []string, fn func(row []Value) bool) error {
	t, def, err := db.lookup(name)
	if err != nil {
		return err
	}
	idx := make([]int, len(cols))
	for i, c := range cols {
		j := slices.IndexFunc(def.cols, func(d colDef) bool { return strings.EqualFold(d.name, c) })
		switch {
		case j < 0:
			return i18n.Errorf("%w: %s.%s", ErrNoColumn, t.name, c)
		case def.cols[j].rowid:
			idx[i] = rowidColumn
		default:
			idx[i] = j
		}
	}
	// 先在 int64 上判范围：直接转 uint32 会把超出的值截断成合法页号。
	if t.root < 1 || t.root > int64(db.p.PageCount()) {
		return i18n.Errorf("%w: invalid root page %d of table %s", ErrCorrupt, t.root, t.name)
	}
	w := walker{p: db.p}
	return w.walk(uint32(t.root), func(rowid int64, payload []byte) (bool, error) {
		row, err := decodeRecord(payload, rowid, idx)
		if err != nil {
			return false, err
		}
		return fn(row), nil
	})
}

// lookup 按名找表并解析建表语句。依次判：没有该表（ErrNoTable）；虚拟表（ErrUnsupportedTable），
// 即根页号为 0 或建表语句以 CREATE VIRTUAL TABLE 开头，先于解析失败判，其参数写法不必能解析；
// 建表语句解析不了（ErrCorrupt）；WITHOUT ROWID 表（ErrUnsupportedTable）。
func (db *DB) lookup(name string) (table, tableDef, error) {
	for _, t := range db.tables {
		if !strings.EqualFold(t.name, name) {
			continue
		}
		def, ok := parseCreateTable(t.sql)
		switch {
		case t.root == 0 || def.virtual:
			return t, def, i18n.Errorf("%w: table %s is a virtual table", ErrUnsupportedTable, t.name)
		case !ok:
			return t, def, i18n.Errorf("%w: cannot parse CREATE TABLE statement of table %s", ErrCorrupt, t.name)
		case def.withoutRowid:
			return t, def, i18n.Errorf("%w: table %s is WITHOUT ROWID", ErrUnsupportedTable, t.name)
		}
		return t, def, nil
	}
	return table{}, tableDef{}, i18n.Errorf("%w: %s", ErrNoTable, name)
}

// walker 遍历表 b-tree，并以库的总页数为一次遍历的取页次数上限（含溢出页）。
// 不记已访问页：内部页成环受深度上限所限，溢出链成环受「读满载荷时下一页须为 0」所限，
// 多个指针指向同一棵子树由取页次数兜住。
type walker struct {
	p      Pager
	visits uint32
}

// page 取第 n 页：先查页号范围与取页次数，Pager 的错误原样返回。
func (w *walker) page(n uint32) ([]byte, error) {
	count := w.p.PageCount()
	if n == 0 || n > count {
		return nil, i18n.Errorf("%w: page %d out of range [1, %d]", ErrCorrupt, n, count)
	}
	if w.visits == count {
		return nil, i18n.Errorf("%w: one scan reads more than %d pages", ErrCorrupt, count)
	}
	w.visits++
	return w.p.Page(n)
}

// btreePage 是页头已校验的表 b-tree 页。
type btreePage struct {
	no    uint32
	b     []byte
	leaf  bool
	ptrs  int    // 单元格指针数组的起点
	cells int    // 单元格数
	right uint32 // 内部页的最右子页
}

func (w *walker) load(n uint32) (btreePage, error) {
	b, err := w.page(n)
	if err != nil {
		return btreePage{}, err
	}
	h := 0
	if n == 1 {
		h = 100 // 第 1 页的页头在文件头之后
	}
	p := btreePage{no: n, b: b, cells: int(binary.BigEndian.Uint16(b[h+3:]))}
	switch b[h] {
	case 0x0D:
		p.leaf, p.ptrs = true, h+8
	case 0x05:
		p.ptrs, p.right = h+12, binary.BigEndian.Uint32(b[h+8:])
	default:
		return p, i18n.Errorf("%w: page %d has invalid b-tree page type 0x%02X", ErrCorrupt, n, b[h])
	}
	return p, nil
}

// cell 返回第 i 个单元格从起点到页尾的字节；起点须在单元格指针数组之后、页内。
// 单元格数大到指针数组越出页面时，任何起点都不满足此条，第 0 个单元格即报错，故不另查单元格数。
func (p *btreePage) cell(i int) ([]byte, error) {
	off := int(binary.BigEndian.Uint16(p.b[p.ptrs+2*i:]))
	if off < p.ptrs+2*p.cells || off >= PageSize {
		return nil, i18n.Errorf("%w: page %d cell %d offset %d out of bounds", ErrCorrupt, p.no, i, off)
	}
	return p.b[off:], nil
}

func (p *btreePage) truncated(i int) error {
	return i18n.Errorf("%w: page %d cell %d is truncated", ErrCorrupt, p.no, i)
}

// walk 以显式栈按 rowid 升序遍历以 root 为根的表 b-tree，对每个叶单元格调用 visit；
// visit 返回 false 或错误即停。栈至多 maxDepth 层，每层持有一页。
func (w *walker) walk(root uint32, visit func(rowid int64, payload []byte) (bool, error)) error {
	type frame struct {
		pg   btreePage
		next int // 下一个要下行的子指针：0..cells-1 为单元格，cells 为最右指针
	}
	var stack []frame
	for child := root; ; {
		if len(stack) == maxDepth {
			return i18n.Errorf("%w: b-tree deeper than %d pages", ErrCorrupt, maxDepth)
		}
		pg, err := w.load(child)
		if err != nil {
			return err
		}
		if !pg.leaf {
			stack = append(stack, frame{pg: pg})
		}
		for i := 0; pg.leaf && i < pg.cells; i++ {
			rowid, payload, err := w.leafCell(&pg, i)
			if err != nil {
				return err
			}
			if ok, err := visit(rowid, payload); err != nil || !ok {
				return err
			}
		}

		for {
			if len(stack) == 0 {
				return nil
			}
			f := &stack[len(stack)-1]
			if f.next > f.pg.cells {
				stack = stack[:len(stack)-1]
				continue
			}
			if f.next == f.pg.cells {
				child = f.pg.right
			} else {
				c, err := f.pg.cell(f.next)
				if err != nil {
					return err
				}
				if len(c) < 4 {
					return f.pg.truncated(f.next)
				}
				child = binary.BigEndian.Uint32(c)
			}
			f.next++
			break
		}
	}
}

// leafCell 解出叶页第 i 个单元格的 rowid 与完整载荷（含溢出页）。
func (w *walker) leafCell(p *btreePage, i int) (int64, []byte, error) {
	c, err := p.cell(i)
	if err != nil {
		return 0, nil, err
	}
	size, n := readVarint(c)
	if n == 0 {
		return 0, nil, p.truncated(i)
	}
	if size > maxRecordBytes {
		return 0, nil, i18n.Errorf("%w: record of %d bytes exceeds the %d-byte limit", ErrCorrupt, size, maxRecordBytes)
	}
	rowid, m := readVarint(c[n:])
	if m == 0 {
		return 0, nil, p.truncated(i)
	}
	c = c[n+m:]
	local := localPayload(size)
	if uint64(len(c)) < local || local < size && uint64(len(c)) < local+4 {
		return 0, nil, p.truncated(i)
	}
	payload := c[:local:local] // 容量封顶：追加溢出内容时另起缓冲区，不写进页
	if local == size {
		return int64(rowid), payload, nil
	}
	// 链在载荷读满之前结束（下一页为 0）由 w.page 的页号范围检查报出。
	next := binary.BigEndian.Uint32(c[local:])
	for uint64(len(payload)) < size {
		ov, err := w.page(next)
		if err != nil {
			return 0, nil, err
		}
		next = binary.BigEndian.Uint32(ov)
		chunk := ov[4:]
		if rest := size - uint64(len(payload)); rest < uint64(len(chunk)) {
			chunk = chunk[:rest]
		}
		payload = append(payload, chunk...)
	}
	if next != 0 {
		return 0, nil, i18n.Errorf("%w: overflow chain does not end after the record", ErrCorrupt)
	}
	return int64(rowid), payload, nil
}

// localPayload 按 SQLite 文件格式求表叶页单元格存在本页的载荷字节数。
// 保留字节为 0（Open 已校验），可用页大小 U 即 PageSize：X = U-35，M = ((U-12)*32/255)-23，
// K = M+((P-M)%(U-4))；P ≤ X 全在本页，否则 K ≤ X 取 K、不然取 M。
func localPayload(p uint64) uint64 {
	const (
		u = PageSize
		x = u - 35
		m = (u-12)*32/255 - 23
	)
	if p <= x {
		return p
	}
	if k := m + (p-m)%(u-4); k <= x {
		return k
	}
	return m
}

// readVarint 解 SQLite 的大端 varint（1～9 字节，第 9 字节取满 8 位）；b 装不下完整的 varint 时 n 为 0。
func readVarint(b []byte) (v uint64, n int) {
	for i := 0; i < len(b) && i < 8; i++ {
		v = v<<7 | uint64(b[i]&0x7F)
		if b[i] < 0x80 {
			return v, i + 1
		}
	}
	if len(b) < 9 {
		return 0, 0
	}
	return v<<8 | uint64(b[8]), 9
}

// rowidColumn 在 decodeRecord 的列下标里表示取 rowid。
const rowidColumn = -1

// decodeRecord 按记录格式解出第 idx[i] 列的值放进 row[i]；idx[i] 为 rowidColumn 取 rowid，
// 记录的列数不足时为 Null。只解析到所需的最后一列。
func decodeRecord(payload []byte, rowid int64, idx []int) ([]Value, error) {
	row := make([]Value, len(idx))
	last := -1
	for i, j := range idx {
		if j == rowidColumn {
			row[i] = Value{Kind: Integer, Int: rowid}
		}
		last = max(last, j)
	}
	hdr, n := readVarint(payload)
	if n == 0 || hdr < uint64(n) || hdr > uint64(len(payload)) {
		return nil, i18n.Errorf("%w: invalid record header", ErrCorrupt)
	}
	types, body := payload[n:hdr], payload[hdr:]
	for col := 0; col <= last && len(types) > 0; col++ {
		st, k := readVarint(types)
		size, ok := serialSize(st)
		if k == 0 || !ok {
			return nil, i18n.Errorf("%w: invalid record header", ErrCorrupt)
		}
		if size > uint64(len(body)) {
			return nil, i18n.Errorf("%w: record value exceeds the payload", ErrCorrupt)
		}
		types = types[k:]
		for i, j := range idx {
			if j == col {
				row[i] = makeValue(st, body[:size])
			}
		}
		body = body[size:]
	}
	return row, nil
}

// serialSize 返回序列类型 st 的值所占字节数；10、11 为保留值，ok 为 false。
func serialSize(st uint64) (size uint64, ok bool) {
	switch {
	case st <= 4:
		return st, true
	case st == 5:
		return 6, true
	case st == 6 || st == 7:
		return 8, true
	case st == 8 || st == 9:
		return 0, true
	case st >= 12:
		return (st - 12) / 2, true
	}
	return 0, false
}

// makeValue 由序列类型与值的字节构造 Value；文本与 BLOB 复制一份。
func makeValue(st uint64, b []byte) Value {
	switch {
	case st == 0:
		return Value{}
	case st <= 6: // 大端补码整数
		v := int64(int8(b[0]))
		for _, c := range b[1:] {
			v = v<<8 | int64(c)
		}
		return Value{Kind: Integer, Int: v}
	case st == 7:
		return Value{Kind: Float, Float: math.Float64frombits(binary.BigEndian.Uint64(b))}
	case st == 8 || st == 9:
		return Value{Kind: Integer, Int: int64(st - 8)}
	case st%2 == 0:
		return Value{Kind: Blob, Bytes: bytes.Clone(b)}
	}
	return Value{Kind: Text, Bytes: bytes.Clone(b)}
}

// tableDef 是由建表语句得出的列序。虚拟表只记 virtual，不取列。
type tableDef struct {
	cols         []colDef
	withoutRowid bool
	virtual      bool
}

type colDef struct {
	name  string
	rowid bool // INTEGER PRIMARY KEY（不含 DESC）：rowid 的别名，记录里存 NULL
}

// parseCreateTable 取建表语句里的列序（设计 §6.10）：第一对顶层括号内按顶层逗号切分，
// 跳过以 CONSTRAINT、PRIMARY、UNIQUE、CHECK、FOREIGN 开头的表约束，列名去引号；括号之后有
// WITHOUT ROWID 即为 WITHOUT ROWID 表。以 CREATE VIRTUAL TABLE 开头的是虚拟表，不再往下解析，ok 为 true。
// 没有列或括号、引号不配对时 ok 为 false。
//
// ponytail: 不处理 SQL 注释与生成列；rowid 别名只认列约束写法「名 INTEGER … PRIMARY KEY」，
// 不认表约束 PRIMARY KEY(名)。酷狗的建表语句不涉及，遇到再补。
func parseCreateTable(sql string) (def tableDef, ok bool) {
	toks, ok := sqlTokens(sql)
	if len(toks) >= 3 && strings.EqualFold(toks[0], "CREATE") && strings.EqualFold(toks[1], "VIRTUAL") &&
		strings.EqualFold(toks[2], "TABLE") {
		return tableDef{virtual: true}, true
	}
	open := slices.Index(toks, "(")
	if !ok || open < 0 {
		return tableDef{}, false
	}
	var item []string
	depth := 1
	for i := open + 1; i < len(toks); i++ {
		switch t := toks[i]; {
		case t == "(":
			depth++
		case t == ")" && depth > 1:
			depth--
		case t == ")" || t == "," && depth == 1:
			if len(item) == 0 {
				return tableDef{}, false
			}
			def.addItem(item)
			item = nil
			if t == ")" {
				def.withoutRowid = seqIndex(toks[i+1:], "WITHOUT", "ROWID") >= 0
				return def, len(def.cols) > 0
			}
			continue
		}
		item = append(item, toks[i])
	}
	return tableDef{}, false
}

// addItem 收入列表中的一项：表约束跳过，其余为列定义。
func (d *tableDef) addItem(item []string) {
	switch strings.ToUpper(item[0]) {
	case "CONSTRAINT", "PRIMARY", "UNIQUE", "CHECK", "FOREIGN":
		return
	}
	c := colDef{name: unquote(item[0])}
	if len(item) > 1 && strings.EqualFold(item[1], "INTEGER") {
		// 列约束 PRIMARY KEY 后紧跟 DESC 时不是 rowid 别名，列里存实际值
		// （https://www.sqlite.org/lang_createtable.html#rowid）；ASC 或无排序关键字时是别名。
		rest := item[2:]
		k := seqIndex(rest, "PRIMARY", "KEY")
		c.rowid = k >= 0 && (k+2 == len(rest) || !strings.EqualFold(rest[k+2], "DESC"))
	}
	d.cols = append(d.cols, c)
}

// sqlTokens 把 SQL 文本切成记号：带引号的标识符或字符串（连同引号，内部的重写引号为转义）、
// 单个括号或逗号、其余以空白和这些符号分隔的词。引号不闭合时 ok 为 false。
func sqlTokens(s string) (toks []string, ok bool) {
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c <= ' ':
			i++
		case c == '(' || c == ')' || c == ',':
			toks = append(toks, s[i:i+1])
			i++
		case c == '"' || c == '\'' || c == '`' || c == '[':
			end := c
			if c == '[' {
				end = ']'
			}
			j := i + 1
			for {
				k := strings.IndexByte(s[j:], end)
				if k < 0 {
					return nil, false
				}
				j += k + 1
				if end == ']' || j == len(s) || s[j] != end {
					break
				}
				j++
			}
			toks = append(toks, s[i:j])
			i = j
		default:
			j := i + 1
			for j < len(s) && s[j] > ' ' && !strings.ContainsRune("(),\"'`[", rune(s[j])) {
				j++
			}
			toks = append(toks, s[i:j])
			i = j
		}
	}
	return toks, true
}

// unquote 去掉 sqlTokens 给出的带引号记号的引号，并还原重写的引号。
func unquote(s string) string {
	switch s[0] {
	case '[':
		return s[1 : len(s)-1]
	case '"', '\'', '`':
		q := s[:1]
		return strings.ReplaceAll(s[1:len(s)-1], q+q, q)
	}
	return s
}

// seqIndex 返回 toks 里第一处相邻的 a、b（不分大小写）中 a 的下标，没有为 -1。
func seqIndex(toks []string, a, b string) int {
	for i := 0; i+1 < len(toks); i++ {
		if strings.EqualFold(toks[i], a) && strings.EqualFold(toks[i+1], b) {
			return i
		}
	}
	return -1
}
