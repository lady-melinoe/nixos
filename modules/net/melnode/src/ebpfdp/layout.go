package ebpfdp

import (
	"encoding/binary"
	"fmt"

	"github.com/cilium/ebpf/btf"
)

// A layout describes where the fields of one C struct live, read from the BTF of the loaded
// object. The helper keeps no copy of the BPF side's structs: whatever the object says is what it
// uses, and a field it needs that is missing (or the wrong size) stops it at start-up.
type layout struct {
	name   string
	size   int
	fields map[string]field
}

type field struct{ off, size int }

func newLayout(spec *btf.Spec, name string, want map[string]int) (*layout, error) {
	var st *btf.Struct
	if err := spec.TypeByName(name, &st); err != nil {
		return nil, fmt.Errorf("object has no struct %s: %w", name, err)
	}
	l := &layout{name: name, size: int(st.Size), fields: make(map[string]field, len(st.Members))}
	for _, m := range st.Members {
		sz, err := btf.Sizeof(m.Type)
		if err != nil {
			return nil, fmt.Errorf("struct %s field %s: %w", name, m.Name, err)
		}
		l.fields[m.Name] = field{off: int(m.Offset.Bytes()), size: sz}
	}
	for f, sz := range want {
		got, ok := l.fields[f]
		if !ok {
			return nil, fmt.Errorf("struct %s has no field %q", name, f)
		}
		if sz != 0 && got.size != sz {
			return nil, fmt.Errorf("struct %s field %q is %d bytes, expected %d", name, f, got.size, sz)
		}
	}
	return l, nil
}

func (l *layout) new() []byte { return make([]byte, l.size) }

func (l *layout) f(name string) field {
	f, ok := l.fields[name]
	if !ok {
		panic(fmt.Sprintf("layout %s: no field %s", l.name, name))
	}
	return f
}

var ne = binary.NativeEndian

func (l *layout) setU8(b []byte, name string, v uint8)   { b[l.f(name).off] = v }
func (l *layout) setU16(b []byte, name string, v uint16) { ne.PutUint16(b[l.f(name).off:], v) }
func (l *layout) setU32(b []byte, name string, v uint32) { ne.PutUint32(b[l.f(name).off:], v) }
func (l *layout) setU64(b []byte, name string, v uint64) { ne.PutUint64(b[l.f(name).off:], v) }

func (l *layout) getU8(b []byte, name string) uint8   { return b[l.f(name).off] }
func (l *layout) getU16(b []byte, name string) uint16 { return ne.Uint16(b[l.f(name).off:]) }
func (l *layout) getU32(b []byte, name string) uint32 { return ne.Uint32(b[l.f(name).off:]) }
func (l *layout) getU64(b []byte, name string) uint64 { return ne.Uint64(b[l.f(name).off:]) }

// bytes returns the field's storage, aliasing b.
func (l *layout) bytes(b []byte, name string) []byte {
	f := l.f(name)
	return b[f.off : f.off+f.size]
}
