package pos

// This file tests only the build admission of the seek-lag correction: the
// executable-section fingerprint, the known-build table, the in-memory header
// consistency check and the bounded file read (design §3.1, §3.6, §4
// "指纹"). Files are synthetic PE32 images, so it runs on any platform.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"testing/iotest"
)

const (
	tpPE      = 0x80
	tpOpt     = tpPE + 24
	tpSecHdr  = tpOpt + 0xe0 // section table
	tpText    = 0x200        // .text raw data offset
	tpRsrc    = 0x400        // .rsrc raw data offset
	tpRawSize = 0x200
	tpFile    = 0x600
	tpImage   = 0x3000
	tpStamp   = 0x6a26414a
	tpExec    = 0x60000020 // CNT_CODE | MEM_EXECUTE | MEM_READ
	tpData    = 0x40000040 // CNT_INITIALIZED_DATA | MEM_READ
)

func put16(b []byte, off int, v uint16) { binary.LittleEndian.PutUint16(b[off:], v) }
func put32(b []byte, off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }

// lagSection writes section header i.
func lagSection(b []byte, i int, name string, rva, raw, chars uint32) {
	h := tpSecHdr + i*40
	copy(b[h:h+8], name)
	put32(b, h+8, tpRawSize)  // VirtualSize
	put32(b, h+12, rva)       // VirtualAddress
	put32(b, h+16, tpRawSize) // SizeOfRawData
	put32(b, h+20, raw)       // PointerToRawData
	put32(b, h+36, chars)
}

// newLagPE builds a minimal PE32 DLL file with an executable .text section
// and a resource section, each tpRawSize bytes of distinct raw data.
func newLagPE() []byte {
	b := make([]byte, tpFile)
	copy(b, "MZ")
	put32(b, 0x3c, tpPE)
	copy(b[tpPE:], "PE\x00\x00")
	put16(b, tpPE+4, 0x14c)   // Machine i386
	put16(b, tpPE+6, 2)       // NumberOfSections
	put32(b, tpPE+8, tpStamp) // TimeDateStamp
	put16(b, tpPE+20, 0xe0)   // SizeOfOptionalHeader
	put16(b, tpPE+22, 0x2102) // Characteristics: executable, 32-bit, DLL
	put16(b, tpOpt, 0x10b)    // Magic PE32
	put32(b, tpOpt+28, 0x10000000)
	put32(b, tpOpt+32, 0x1000) // SectionAlignment
	put32(b, tpOpt+36, 0x200)  // FileAlignment
	put32(b, tpOpt+56, tpImage)
	put32(b, tpOpt+60, 0x200) // SizeOfHeaders
	put32(b, tpOpt+92, 16)    // NumberOfRvaAndSizes
	lagSection(b, 0, ".text", 0x1000, tpText, tpExec)
	lagSection(b, 1, ".rsrc", 0x2000, tpRsrc, tpData)
	for i := 0; i < tpRawSize; i++ {
		b[tpText+i] = byte(i*7 + 1)
		b[tpRsrc+i] = byte(i*13 + 5)
	}
	return b
}

// lagMemHeader is the first 0x1000 bytes of the image mapped from file.
func lagMemHeader(file []byte) []byte {
	m := make([]byte, 0x1000)
	copy(m, file[:0x200])
	return m
}

func sha256Hex(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestExecSectionHashIsExecutableRawData(t *testing.T) {
	f := newLagPE()
	got, err := execSectionHash(f)
	if err != nil {
		t.Fatal(err)
	}
	if want := sha256Hex(f[tpText : tpText+tpRawSize]); got != want {
		t.Fatalf("hash %s, want SHA-256 of .text raw data %s", got, want)
	}
}

// Sections are concatenated in section table order, not by address.
func TestExecSectionHashConcatenatesInTableOrder(t *testing.T) {
	f := newLagPE()
	lagSection(f, 1, ".text2", 0x2000, tpRsrc, tpExec)
	text, text2 := f[tpText:tpText+tpRawSize], f[tpRsrc:tpRsrc+tpRawSize]
	if got, _ := execSectionHash(f); got != sha256Hex(text, text2) {
		t.Fatalf("two executable sections: hash %s, want SHA-256 of .text || .text2", got)
	}
	// The second table entry now has the lower address.
	lagSection(f, 0, ".text2", 0x2000, tpRsrc, tpExec)
	lagSection(f, 1, ".text", 0x1000, tpText, tpExec)
	if got, _ := execSectionHash(f); got != sha256Hex(text2, text) {
		t.Fatalf("swapped table entries: hash %s, want SHA-256 of .text2 || .text", got)
	}
}

func TestExecSectionHashIgnoresNonCodeChanges(t *testing.T) {
	base, err := execSectionHash(newLagPE())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"TimeDateStamp", func(b []byte) []byte { put32(b, tpPE+8, tpStamp+1); return b }},
		{"CheckSum", func(b []byte) []byte { put32(b, tpOpt+64, 0x1234); return b }},
		{"resource first byte", func(b []byte) []byte { b[tpRsrc]++; return b }},
		{"resource last byte", func(b []byte) []byte { b[tpRsrc+tpRawSize-1]++; return b }},
		{"appended signature", func(b []byte) []byte { return append(b, bytes.Repeat([]byte{0xa5}, 0x100)...) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := execSectionHash(tc.mutate(newLagPE()))
			if err != nil || got != base {
				t.Fatalf("hash %s, %v; want unchanged %s", got, err, base)
			}
		})
	}
}

func TestExecSectionHashChangesWithCode(t *testing.T) {
	base, _ := execSectionHash(newLagPE())
	for _, off := range []int{tpText, tpText + 0x100, tpText + tpRawSize - 1} {
		f := newLagPE()
		f[off] ^= 0x01
		if got, err := execSectionHash(f); err != nil || got == base {
			t.Errorf("code byte %#x changed: hash %s, %v; want a different hash", off, got, err)
		}
	}
}

// coffObject is a COFF object file with an executable section but no MZ stub
// and no optional header, which debug/pe parses without error.
func coffObject() []byte {
	b := make([]byte, 0x400)
	put16(b, 0, 0x14c) // Machine i386
	put16(b, 2, 1)     // NumberOfSections
	h := 20
	copy(b[h:h+8], ".text")
	put32(b, h+16, tpRawSize) // SizeOfRawData
	put32(b, h+20, 0x200)     // PointerToRawData
	put32(b, h+36, tpExec)
	return b
}

func TestExecSectionHashRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		file func() []byte
	}{
		{"no executable section", func() []byte { f := newLagPE(); lagSection(f, 0, ".text", 0x1000, tpText, tpData); return f }},
		{"COFF object, no optional header", coffObject},
		{"bad PE signature", func() []byte { f := newLagPE(); copy(f[tpPE:], "PX"); return f }},
		{"not MZ", func() []byte { f := newLagPE(); copy(f, "XY"); return f }},
		{"PE32+ magic", func() []byte { f := newLagPE(); put16(f, tpOpt, 0x20b); return f }},
		{"truncated headers", func() []byte { return newLagPE()[:0x100] }},
		{"empty", func() []byte { return nil }},
		{"code past end of file", func() []byte { f := newLagPE(); lagSection(f, 0, ".text", 0x1000, tpFile-0x100, tpExec); return f }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := execSectionHash(tc.file()); err == nil {
				t.Fatalf("accepted: hash %s", got)
			}
		})
	}
}

// The coffObject rejection must come from the PE32 rule: the same section
// layout inside a PE32 image is accepted.
func TestExecSectionHashCOFFCaseIsOtherwiseValid(t *testing.T) {
	f := newLagPE()
	put16(f, tpPE+6, 1)
	if _, err := execSectionHash(f); err != nil {
		t.Fatalf("PE32 image with one executable section rejected: %v", err)
	}
}

func TestMatchLagBuild(t *testing.T) {
	const in, out = "aaaf07c46259aa24941611a15ee3fb8d7a85fc9675053958d8268c38756ad9d8", "2e9ff2f5457e7540279ea4815316874c81e323c34960fd5d9f346827dc9c3677"
	if name, ok := matchLagBuild(in, out); !ok || name != "9.6.0.0_UG1" {
		t.Fatalf("known pair: %q, %v", name, ok)
	}
	other := strings.Repeat("0", 64)
	for _, tc := range []struct{ name, in, out string }{
		{"swapped", out, in},
		{"unknown out", in, other},
		{"unknown in", other, out},
		{"uppercase", strings.ToUpper(in), strings.ToUpper(out)},
		{"empty", "", ""},
	} {
		if name, ok := matchLagBuild(tc.in, tc.out); ok {
			t.Errorf("%s: accepted as %q", tc.name, name)
		}
	}
}

// Both hashes must come from the same row.
func TestMatchLagBuildRequiresOneRow(t *testing.T) {
	saved := lagBuilds
	defer func() { lagBuilds = saved }()
	lagBuilds = []lagBuild{{"A", "in-a", "out-a"}, {"B", "in-b", "out-b"}}
	for _, tc := range []struct {
		in, out, want string
		ok            bool
	}{
		{"in-a", "out-a", "A", true},
		{"in-b", "out-b", "B", true},
		{"in-a", "out-b", "", false},
		{"in-b", "out-a", "", false},
	} {
		if name, ok := matchLagBuild(tc.in, tc.out); ok != tc.ok || name != tc.want {
			t.Errorf("(%s, %s) = %q, %v; want %q, %v", tc.in, tc.out, name, ok, tc.want, tc.ok)
		}
	}
}

// lagBuilds must equal the design §3.1 table, character for character.
func TestLagFingerprintTableMatchesDesign(t *testing.T) {
	doc, err := os.ReadFile("../../../doc/design/kuwo-seek-lag-design.md")
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]map[string]string{} // build → module → hash
	for _, line := range strings.Split(string(doc), "\n") {
		f := strings.Split(line, "|")
		if len(f) != 5 {
			continue
		}
		mod, build, hash := strings.TrimSpace(f[1]), strings.TrimSpace(f[2]), strings.Trim(strings.TrimSpace(f[3]), "`")
		if mod != "in_ffaudio.dll" && mod != "out_kw_ds.dll" {
			continue
		}
		if rows[build] == nil {
			rows[build] = map[string]string{}
		}
		rows[build][mod] = hash
	}
	if len(rows) == 0 || len(rows) != len(lagBuilds) {
		t.Fatalf("design table has %d builds, lagBuilds %d", len(rows), len(lagBuilds))
	}
	for _, b := range lagBuilds {
		r := rows[b.Name]
		if len(r) != 2 || r["in_ffaudio.dll"] != b.InHash || r["out_kw_ds.dll"] != b.OutHash {
			t.Errorf("build %s: design table %v, lagBuilds in %s out %s", b.Name, r, b.InHash, b.OutHash)
		}
	}
}

func TestCheckImageHeaderAccepts(t *testing.T) {
	f := newLagPE()
	if err := checkImageHeader(lagMemHeader(f), f, tpImage); err != nil {
		t.Fatalf("consistent image rejected: %v", err)
	}
	// Code bytes are not part of the header check.
	g := newLagPE()
	g[tpText]++
	if err := checkImageHeader(lagMemHeader(f), g, tpImage); err != nil {
		t.Fatalf("header-equal file rejected: %v", err)
	}
}

// movePE points e_lfanew at p and writes the PE signature and PE32 magic there.
func movePE(b []byte, p int) {
	put32(b, 0x3c, uint32(p))
	copy(b[p:], "PE\x00\x00")
	put16(b, p+0x18, 0x10b)
}

// Each case breaks one rule of an otherwise consistent image.
func TestCheckImageHeaderRejects(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mem     func(m []byte)
		file    func(f []byte)
		modSize uint32
	}{
		{"TimeDateStamp in memory", func(m []byte) { put32(m, tpPE+8, tpStamp+1) }, nil, tpImage},
		{"TimeDateStamp in file", nil, func(f []byte) { put32(f, tpPE+8, tpStamp-1) }, tpImage},
		// ModBaseSize follows memory, so only the file comparison can reject.
		{"SizeOfImage differs from file", func(m []byte) { put32(m, tpOpt+56, tpImage+0x1000) }, nil, tpImage + 0x1000},
		{"SizeOfImage differs from ModBaseSize", nil, nil, tpImage + 0x1000},
		{"memory not MZ", func(m []byte) { copy(m, "XY") }, nil, tpImage},
		{"memory bad PE signature", func(m []byte) { copy(m[tpPE:], "PX") }, nil, tpImage},
		{"memory PE32+ magic", func(m []byte) { put16(m, tpOpt, 0x20b) }, nil, tpImage},
		// A valid signature and magic, with SizeOfImage just past the end:
		// only the bound may reject.
		{"memory SizeOfImage past page", func(m []byte) { movePE(m, 0x1000-0x50) }, nil, tpImage},
		{"memory e_lfanew huge", func(m []byte) { put32(m, 0x3c, 0xffffffff) }, nil, tpImage},
		{"file not MZ", nil, func(f []byte) { copy(f, "XY") }, tpImage},
		{"file SizeOfImage past end", nil, func(f []byte) { movePE(f, tpFile-0x50) }, tpImage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLagPE()
			m := lagMemHeader(f)
			if tc.mem != nil {
				tc.mem(m)
			}
			if tc.file != nil {
				tc.file(f)
			}
			if err := checkImageHeader(m, f, tc.modSize); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	if err := checkImageHeader(nil, newLagPE(), tpImage); err == nil {
		t.Error("empty memory header accepted")
	}
}

// SizeOfImage ending exactly at the end of the buffer is read; one byte
// further is rejected.
func TestCheckImageHeaderBoundEdge(t *testing.T) {
	b := make([]byte, 0x1000)
	copy(b, "MZ")
	p := 0x1000 - 0x54
	movePE(b, p)
	put32(b, p+8, tpStamp)
	put32(b, p+0x50, tpImage)
	if stamp, size, err := peStampSize(b); err != nil || stamp != tpStamp || size != tpImage {
		t.Fatalf("at the edge: %#x, %#x, %v", stamp, size, err)
	}
	movePE(b, p+1)
	if _, _, err := peStampSize(b); err == nil {
		t.Fatal("one byte past the edge accepted")
	}
}

func TestReadLimited(t *testing.T) {
	if lagFileLimit != 4<<20 {
		t.Fatalf("lagFileLimit = %d, want 4 MiB", lagFileLimit)
	}
	b, err := readLimited(bytes.NewReader(make([]byte, 4<<20)), lagFileLimit)
	if err != nil || len(b) != 4<<20 {
		t.Fatalf("4 MiB: %d bytes, %v; want accepted", len(b), err)
	}
	if b, err := readLimited(bytes.NewReader(make([]byte, 4<<20+1)), lagFileLimit); err == nil {
		t.Fatalf("4 MiB + 1: %d bytes accepted", len(b))
	}
	if b, err := readLimited(bytes.NewReader(nil), lagFileLimit); err != nil || len(b) != 0 {
		t.Fatalf("empty: %d bytes, %v", len(b), err)
	}
	if _, err := readLimited(iotest.ErrReader(errFakeRead), lagFileLimit); err == nil {
		t.Fatal("read error swallowed")
	}
}
