//go:build wazero && !(js && wasm)

package js

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/tetratelabs/wazero/api"
)

// wasmKind is the external kind of a WebAssembly import or export.
type wasmKind byte

const (
	wasmKindFunc   wasmKind = 0x00
	wasmKindTable  wasmKind = 0x01
	wasmKindMemory wasmKind = 0x02
	wasmKindGlobal wasmKind = 0x03
	wasmKindTag    wasmKind = 0x04
)

func wasmKindName(k wasmKind) string {
	switch k {
	case wasmKindFunc:
		return "function"
	case wasmKindTable:
		return "table"
	case wasmKindMemory:
		return "memory"
	case wasmKindGlobal:
		return "global"
	case wasmKindTag:
		return "tag"
	default:
		return "unknown"
	}
}

// wasmLimits is a memory or table limits descriptor.
type wasmLimits struct {
	min    uint32
	max    uint32
	hasMax bool
	shared bool
}

// wasmGlobalInfo is a global's value type and mutability.
type wasmGlobalInfo struct {
	valueType api.ValueType
	mutable   bool
}

// wasmTableInfo is a table's element type and limits.
type wasmTableInfo struct {
	elementType api.ValueType
	limits      wasmLimits
}

// wasmImportInfo is one parsed import.
type wasmImportInfo struct {
	module    string
	name      string
	kind      wasmKind
	typeIndex uint32
	valueType api.ValueType
	mutable   bool
	limits    wasmLimits
}

// wasmExportInfo is one parsed export.
type wasmExportInfo struct {
	name  string
	kind  wasmKind
	index uint32
}

// wasmBinaryInfo is the subset of a WebAssembly module the JS API exposes.
type wasmBinaryInfo struct {
	imports  []wasmImportInfo
	exports  []wasmExportInfo
	tables   []wasmTableInfo
	memories []wasmLimits
	globals  []wasmGlobalInfo
	hasStart bool
}

// parseWasmBinary reads the import, export, table, memory and global sections
// of a binary that wazero has already validated. It does not interpret code.
func parseWasmBinary(b []byte) (*wasmBinaryInfo, error) {
	if len(b) < 8 || b[0] != 0x00 || b[1] != 0x61 || b[2] != 0x73 || b[3] != 0x6d {
		return nil, errors.New("canopy: invalid WebAssembly binary: bad magic")
	}
	if b[4] != 0x01 || b[5] != 0x00 || b[6] != 0x00 || b[7] != 0x00 {
		return nil, errors.New("canopy: invalid WebAssembly binary: unsupported version")
	}
	info := &wasmBinaryInfo{}
	r := wasmReader{b: b, pos: 8}
	for r.pos < len(r.b) {
		id, err := r.u8()
		if err != nil {
			return nil, err
		}
		size, err := r.u32()
		if err != nil {
			return nil, err
		}
		start := r.pos
		end := start + int(size)
		if end < start || end > len(r.b) {
			return nil, errors.New("canopy: invalid WebAssembly binary: section overruns the binary")
		}
		sec := wasmReader{b: r.b[:end], pos: start}
		switch id {
		case 1: // type
			// Function signatures come from wazero's definitions.
		case 2:
			if err := parseWasmImports(&sec, info); err != nil {
				return nil, err
			}
		case 3: // function
			// Defined function type indices are not needed for the JS API.
		case 4:
			if err := parseWasmTables(&sec, info); err != nil {
				return nil, err
			}
		case 5:
			if err := parseWasmMemories(&sec, info); err != nil {
				return nil, err
			}
		case 6:
			if err := parseWasmGlobals(&sec, info); err != nil {
				return nil, err
			}
		case 7:
			if err := parseWasmExports(&sec, info); err != nil {
				return nil, err
			}
		case 8:
			info.hasStart = true
		}
		r.pos = end
	}
	return info, nil
}

func parseWasmImports(r *wasmReader, info *wasmBinaryInfo) error {
	n, err := r.u32()
	if err != nil {
		return err
	}
	for i := uint32(0); i < n; i++ {
		module, err := r.str()
		if err != nil {
			return err
		}
		name, err := r.str()
		if err != nil {
			return err
		}
		kindByte, err := r.u8()
		if err != nil {
			return err
		}
		imp := wasmImportInfo{module: module, name: name, kind: wasmKind(kindByte)}
		switch imp.kind {
		case wasmKindFunc:
			idx, err := r.u32()
			if err != nil {
				return err
			}
			imp.typeIndex = idx
		case wasmKindTable:
			elem, err := r.u8()
			if err != nil {
				return err
			}
			lim, err := r.limits()
			if err != nil {
				return err
			}
			imp.valueType = api.ValueType(elem)
			imp.limits = lim
			info.tables = append(info.tables, wasmTableInfo{elementType: api.ValueType(elem), limits: lim})
		case wasmKindMemory:
			lim, err := r.limits()
			if err != nil {
				return err
			}
			imp.limits = lim
			info.memories = append(info.memories, lim)
		case wasmKindGlobal:
			vt, err := r.u8()
			if err != nil {
				return err
			}
			mut, err := r.u8()
			if err != nil {
				return err
			}
			imp.valueType = api.ValueType(vt)
			imp.mutable = mut != 0
			info.globals = append(info.globals, wasmGlobalInfo{valueType: imp.valueType, mutable: imp.mutable})
		case wasmKindTag:
			if _, err := r.u8(); err != nil {
				return err
			}
			if _, err := r.u32(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("canopy: invalid WebAssembly binary: import %d has kind %d", i, kindByte)
		}
		info.imports = append(info.imports, imp)
	}
	return nil
}

func parseWasmTables(r *wasmReader, info *wasmBinaryInfo) error {
	n, err := r.u32()
	if err != nil {
		return err
	}
	for i := uint32(0); i < n; i++ {
		elem, err := r.u8()
		if err != nil {
			return err
		}
		lim, err := r.limits()
		if err != nil {
			return err
		}
		info.tables = append(info.tables, wasmTableInfo{elementType: api.ValueType(elem), limits: lim})
	}
	return nil
}

func parseWasmMemories(r *wasmReader, info *wasmBinaryInfo) error {
	n, err := r.u32()
	if err != nil {
		return err
	}
	for i := uint32(0); i < n; i++ {
		lim, err := r.limits()
		if err != nil {
			return err
		}
		info.memories = append(info.memories, lim)
	}
	return nil
}

func parseWasmGlobals(r *wasmReader, info *wasmBinaryInfo) error {
	n, err := r.u32()
	if err != nil {
		return err
	}
	for i := uint32(0); i < n; i++ {
		vt, err := r.u8()
		if err != nil {
			return err
		}
		mut, err := r.u8()
		if err != nil {
			return err
		}
		if err := r.skipInitExpr(); err != nil {
			return err
		}
		info.globals = append(info.globals, wasmGlobalInfo{valueType: api.ValueType(vt), mutable: mut != 0})
	}
	return nil
}

func parseWasmExports(r *wasmReader, info *wasmBinaryInfo) error {
	n, err := r.u32()
	if err != nil {
		return err
	}
	for i := uint32(0); i < n; i++ {
		name, err := r.str()
		if err != nil {
			return err
		}
		kindByte, err := r.u8()
		if err != nil {
			return err
		}
		idx, err := r.u32()
		if err != nil {
			return err
		}
		info.exports = append(info.exports, wasmExportInfo{name: name, kind: wasmKind(kindByte), index: idx})
	}
	return nil
}

// wasmReader reads the little-endian/LEB128 encoding of a wasm binary.
type wasmReader struct {
	b   []byte
	pos int
}

func (r *wasmReader) u8() (byte, error) {
	if r.pos >= len(r.b) {
		return 0, errors.New("canopy: invalid WebAssembly binary: unexpected end")
	}
	v := r.b[r.pos]
	r.pos++
	return v, nil
}

func (r *wasmReader) u32() (uint32, error) {
	var v uint32
	var shift uint
	for i := 0; i < 5; i++ {
		b, err := r.u8()
		if err != nil {
			return 0, err
		}
		v |= uint32(b&0x7f) << shift
		if b&0x80 == 0 {
			return v, nil
		}
		shift += 7
	}
	return 0, errors.New("canopy: invalid WebAssembly binary: LEB128 is too long")
}

func (r *wasmReader) str() (string, error) {
	n, err := r.u32()
	if err != nil {
		return "", err
	}
	if int(n) > len(r.b)-r.pos {
		return "", errors.New("canopy: invalid WebAssembly binary: string overruns section")
	}
	s := string(r.b[r.pos : r.pos+int(n)])
	r.pos += int(n)
	return s, nil
}

func (r *wasmReader) limits() (wasmLimits, error) {
	flags, err := r.u32()
	if err != nil {
		return wasmLimits{}, err
	}
	lim := wasmLimits{shared: flags&0x02 != 0}
	lim.min, err = r.u32()
	if err != nil {
		return wasmLimits{}, err
	}
	if flags&0x01 != 0 {
		lim.hasMax = true
		lim.max, err = r.u32()
		if err != nil {
			return wasmLimits{}, err
		}
	}
	return lim, nil
}

func (r *wasmReader) skipLEB() error {
	for {
		b, err := r.u8()
		if err != nil {
			return err
		}
		if b&0x80 == 0 {
			return nil
		}
	}
}

// skipInitExpr skips a constant expression. The operand types are the ones
// enabled by CoreFeaturesV2 and the constant-expression allowed by it.
func (r *wasmReader) skipInitExpr() error {
	for {
		op, err := r.u8()
		if err != nil {
			return err
		}
		switch op {
		case 0x0b: // end
			return nil
		case 0x41, 0x42, 0x23, 0xd2: // i32.const, i64.const, global.get, ref.func
			if err := r.skipLEB(); err != nil {
				return err
			}
		case 0x43: // f32.const
			if _, err := r.bytes(4); err != nil {
				return err
			}
		case 0x44: // f64.const
			if _, err := r.bytes(8); err != nil {
				return err
			}
		case 0xd0: // ref.null
			if _, err := r.u8(); err != nil {
				return err
			}
		case 0x6a, 0x6b, 0x6c, 0x7c, 0x7d, 0x7e: // extended-const arithmetic
			// no immediates
		default:
			return fmt.Errorf("canopy: invalid WebAssembly binary: unsupported constant expression opcode 0x%02x", op)
		}
	}
}

func (r *wasmReader) bytes(n int) ([]byte, error) {
	if n < 0 || n > len(r.b)-r.pos {
		return nil, errors.New("canopy: invalid WebAssembly binary: value overruns section")
	}
	v := r.b[r.pos : r.pos+n]
	r.pos += n
	return v, nil
}

// wasmFuncSig is a function signature used by the synthetic import modules.
type wasmFuncSig struct {
	params  []api.ValueType
	results []api.ValueType
}

func (s wasmFuncSig) key() string {
	var b strings.Builder
	b.WriteByte(byte(len(s.params)))
	for _, t := range s.params {
		b.WriteByte(byte(t))
	}
	b.WriteByte(byte(len(s.results)))
	for _, t := range s.results {
		b.WriteByte(byte(t))
	}
	return b.String()
}

// wasmModuleBuilder writes the small synthetic modules used to expose host
// memories/globals and to make function imports visible under their original
// module names.
type wasmModuleBuilder struct {
	types     []wasmFuncSig
	typeIndex map[string]uint32
	imports   []wasmImportSpec
	tables    []wasmTableInfo
	memories  []wasmLimits
	globals   []wasmGlobalSpec
	exports   []wasmExportSpec
}

type wasmImportSpec struct {
	module    string
	name      string
	kind      wasmKind
	typeIndex uint32
	valueType api.ValueType
	mutable   bool
	limits    wasmLimits
}

type wasmExportSpec struct {
	name  string
	kind  wasmKind
	index uint32
}

type wasmGlobalSpec struct {
	valueType api.ValueType
	mutable   bool
	init      []byte // constant expression without the trailing end opcode
}

func (b *wasmModuleBuilder) addType(sig wasmFuncSig) uint32 {
	if b.typeIndex == nil {
		b.typeIndex = make(map[string]uint32)
	}
	k := sig.key()
	if i, ok := b.typeIndex[k]; ok {
		return i
	}
	i := uint32(len(b.types))
	b.types = append(b.types, sig)
	b.typeIndex[k] = i
	return i
}

func (b *wasmModuleBuilder) build() []byte {
	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	if len(b.types) > 0 {
		var p []byte
		p = appendU32(p, uint32(len(b.types)))
		for _, t := range b.types {
			p = append(p, 0x60)
			p = appendU32(p, uint32(len(t.params)))
			for _, vt := range t.params {
				p = append(p, byte(vt))
			}
			p = appendU32(p, uint32(len(t.results)))
			for _, vt := range t.results {
				p = append(p, byte(vt))
			}
		}
		out = append(out, wasmSection(1, p)...)
	}
	if len(b.imports) > 0 {
		var p []byte
		p = appendU32(p, uint32(len(b.imports)))
		for _, imp := range b.imports {
			p = appendString(p, imp.module)
			p = appendString(p, imp.name)
			p = append(p, byte(imp.kind))
			switch imp.kind {
			case wasmKindFunc:
				p = appendU32(p, imp.typeIndex)
			case wasmKindTable:
				p = append(p, byte(imp.valueType))
				p = appendLimits(p, imp.limits)
			case wasmKindMemory:
				p = appendLimits(p, imp.limits)
			case wasmKindGlobal:
				p = append(p, byte(imp.valueType))
				p = append(p, boolByte(imp.mutable))
			}
		}
		out = append(out, wasmSection(2, p)...)
	}
	if len(b.tables) > 0 {
		var p []byte
		p = appendU32(p, uint32(len(b.tables)))
		for _, t := range b.tables {
			p = append(p, byte(t.elementType))
			p = appendLimits(p, t.limits)
		}
		out = append(out, wasmSection(4, p)...)
	}
	if len(b.memories) > 0 {
		var p []byte
		p = appendU32(p, uint32(len(b.memories)))
		for _, m := range b.memories {
			p = appendLimits(p, m)
		}
		out = append(out, wasmSection(5, p)...)
	}
	if len(b.globals) > 0 {
		var p []byte
		p = appendU32(p, uint32(len(b.globals)))
		for _, g := range b.globals {
			p = append(p, byte(g.valueType), boolByte(g.mutable))
			p = append(p, g.init...)
			p = append(p, 0x0b)
		}
		out = append(out, wasmSection(6, p)...)
	}
	if len(b.exports) > 0 {
		var p []byte
		p = appendU32(p, uint32(len(b.exports)))
		for _, e := range b.exports {
			p = appendString(p, e.name)
			p = append(p, byte(e.kind))
			p = appendU32(p, e.index)
		}
		out = append(out, wasmSection(7, p)...)
	}
	return out
}

func wasmSection(id byte, payload []byte) []byte {
	out := []byte{id}
	out = appendU32(out, uint32(len(payload)))
	return append(out, payload...)
}

func appendU32(dst []byte, v uint32) []byte {
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			dst = append(dst, b|0x80)
		} else {
			return append(dst, b)
		}
	}
}

func appendString(dst []byte, s string) []byte {
	dst = appendU32(dst, uint32(len(s)))
	return append(dst, s...)
}

func appendLimits(dst []byte, l wasmLimits) []byte {
	var flags uint32
	if l.hasMax {
		flags |= 0x01
	}
	if l.shared {
		flags |= 0x02
	}
	dst = appendU32(dst, flags)
	dst = appendU32(dst, l.min)
	if l.hasMax {
		dst = appendU32(dst, l.max)
	}
	return dst
}

func boolByte(v bool) byte {
	if v {
		return 1
	}
	return 0
}

func appendSLEB(dst []byte, v int64) []byte {
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if (v == 0 && b&0x40 == 0) || (v == -1 && b&0x40 != 0) {
			return append(dst, b)
		}
		dst = append(dst, b|0x80)
	}
}

func wasmConstI32(v int32) []byte {
	return appendSLEB([]byte{0x41}, int64(v))
}

func wasmConstI64(v int64) []byte {
	return appendSLEB([]byte{0x42}, v)
}

func wasmConstF32(v float32) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(api.EncodeF32(v)))
	return append([]byte{0x43}, b[:]...)
}

func wasmConstF64(v float64) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], api.EncodeF64(v))
	return append([]byte{0x44}, b[:]...)
}
