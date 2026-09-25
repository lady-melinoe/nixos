package dpproto

// nl.go: literal Linux (generic) netlink message framing.
//
// The data plane API is defined as a generic netlink family, "melnode", so the
// control plane can talk to a kernel-module data plane with the same messages
// it sends the userspace one. This file is the wire format shared by both:
//
//	struct nlmsghdr    { u32 len; u16 type; u16 flags; u32 seq; u32 pid; }
//	struct genlmsghdr  { u8 cmd; u8 version; u16 reserved; }
//	struct nlattr      { u16 len; u16 type; }  + payload, padded to 4 bytes
//
// All integers are host byte order, exactly as in the kernel. The userspace
// data plane carries these messages over a unix SOCK_SEQPACKET socket (one
// message per datagram); a kernel data plane would carry them over an
// AF_NETLINK / NETLINK_GENERIC socket. Only the transport differs.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

var nativeEndian = binary.NativeEndian

const (
	nlmsgHdrLen  = 16
	genlHdrLen   = 4
	nlattrHdrLen = 4
	// HeaderLen is nlmsghdr + genlmsghdr: everything before the attributes.
	HeaderLen = nlmsgHdrLen + genlHdrLen
	// MaxMessage bounds one datagram: a full 64KiB payload plus headers.
	MaxMessage = 65535 + 256
)

// Netlink message types below the family id range (linux/netlink.h).
const (
	nlmsgNoop  = 1
	nlmsgError = 2
	nlmsgDone  = 3
)

// Netlink message flags (linux/netlink.h).
const (
	nlmFRequest = 0x01
	nlmFMulti   = 0x02
	nlmFAck     = 0x04
	nlmFRoot    = 0x100
	nlmFMatch   = 0x200
	nlmFDump    = nlmFRoot | nlmFMatch
	// On an error message: the original request is truncated to its header,
	// and extended-ack attributes follow (NLM_F_CAPPED, NLM_F_ACK_TLVS).
	nlmFCapped  = 0x100
	nlmFAckTLVs = 0x200
)

// nlattr type flags and the extended-ack attribute (linux/netlink.h).
const (
	nlaFNested      = 1 << 15
	nlaFNetOrder    = 1 << 14
	nlaTypeMask     = ^uint16(nlaFNested | nlaFNetOrder)
	nlmsgerrAttrMsg = 1
)

// The generic netlink controller: a family named "nlctrl" with the fixed id
// 16 that maps family names to their (dynamically assigned) ids. Every genl
// client starts by asking it about the family it wants (CTRL_CMD_GETFAMILY).
const (
	genlIDCtrl = 0x10

	ctrlCmdNewFamily = 1
	ctrlCmdGetFamily = 3
	ctrlVersion      = 2

	ctrlAttrFamilyID    = 1 // u16
	ctrlAttrFamilyName  = 2 // string
	ctrlAttrVersion     = 3 // u32
	ctrlAttrHdrSize     = 4 // u32
	ctrlAttrMaxAttr     = 5 // u32
	ctrlAttrMcastGroups = 7 // nested: one nested entry per group

	ctrlAttrMcastGrpName = 1 // string
	ctrlAttrMcastGrpID   = 2 // u32
)

// FamilyName is the family's registered name. FamilyID is the id the
// userspace data plane gives it. A kernel family gets whatever id genetlink
// hands out at registration, which is why clients never hardcode it: Dial
// resolves it with CTRL_CMD_GETFAMILY exactly as they would against the
// kernel, and the userspace data plane answers that query itself. (198, like
// the routing protocol number, is arbitrary: any id from 16 up is valid.)
const (
	FamilyName = "melnode"
	FamilyID   = 198

	// eventsGroupID is the id the userspace data plane reports for the
	// "events" multicast group. Only meaningful to a kernel backend, which
	// joins the group; here events go to the attached session.
	eventsGroupName = "events"
	eventsGroupID   = 1
)

func align4(n int) int { return (n + 3) &^ 3 }

// ---- building ---------------------------------------------------------------------

// nlb builds one netlink message.
type nlb struct{ b []byte }

func newNL(typ, flags uint16, seq, pid uint32, cmd, version uint8) *nlb {
	m := &nlb{b: make([]byte, HeaderLen, 128)}
	nativeEndian.PutUint16(m.b[4:], typ)
	nativeEndian.PutUint16(m.b[6:], flags)
	nativeEndian.PutUint32(m.b[8:], seq)
	nativeEndian.PutUint32(m.b[12:], pid)
	m.b[16] = cmd
	m.b[17] = version
	return m
}

// bytes finalizes and returns the message.
func (m *nlb) bytes() []byte {
	nativeEndian.PutUint32(m.b[0:], uint32(len(m.b)))
	return m.b
}

func (m *nlb) attr(typ uint16, data []byte) *nlb {
	var h [nlattrHdrLen]byte
	nativeEndian.PutUint16(h[0:], uint16(nlattrHdrLen+len(data)))
	nativeEndian.PutUint16(h[2:], typ)
	m.b = append(m.b, h[:]...)
	m.b = append(m.b, data...)
	for len(m.b)%4 != 0 {
		m.b = append(m.b, 0)
	}
	return m
}

func (m *nlb) u8(typ uint16, v uint8) *nlb { return m.attr(typ, []byte{v}) }
func (m *nlb) u16(typ uint16, v uint16) *nlb {
	return m.attr(typ, nativeEndian.AppendUint16(nil, v))
}
func (m *nlb) u32(typ uint16, v uint32) *nlb {
	return m.attr(typ, nativeEndian.AppendUint32(nil, v))
}

// u64 is nla_put_64bit: a 64-bit attribute's payload must be 8-byte aligned,
// so a header-only padding attribute is emitted first when it wouldn't be.
func (m *nlb) u64(typ uint16, v uint64) *nlb {
	if (len(m.b)+nlattrHdrLen)%8 != 0 {
		m.attr(AttrPad, nil)
	}
	return m.attr(typ, nativeEndian.AppendUint64(nil, v))
}

// nest appends a nested attribute, filled in by fill.
func (m *nlb) nest(typ uint16, fill func(*nlb)) *nlb {
	start := len(m.b)
	m.attr(typ|nlaFNested, nil)
	fill(m)
	nativeEndian.PutUint16(m.b[start:], uint16(len(m.b)-start))
	return m
}

// str is a NUL-terminated string attribute (NLA_NUL_STRING).
func (m *nlb) str(typ uint16, s string) *nlb {
	return m.attr(typ, append([]byte(s), 0))
}

// ---- parsing ----------------------------------------------------------------------

var errShort = errors.New("dpproto: truncated netlink message")

// nlmsg is one parsed netlink message. Attribute payloads alias the buffer it
// was parsed from.
type nlmsg struct {
	typ, flags uint16
	seq, pid   uint32
	cmd        uint8
	version    uint8
	body       []byte // after nlmsghdr; for generic messages includes genlmsghdr
}

func (m *nlmsg) attrs() (attrs, error) {
	if len(m.body) < genlHdrLen {
		return nil, errShort
	}
	return parseAttrs(m.body[genlHdrLen:])
}

// splitMessages parses a datagram as one or more netlink messages.
func splitMessages(b []byte) ([]nlmsg, error) {
	var out []nlmsg
	for len(b) > 0 {
		if len(b) < nlmsgHdrLen {
			return out, errShort
		}
		n := int(nativeEndian.Uint32(b[0:]))
		if n < nlmsgHdrLen || n > len(b) {
			return out, fmt.Errorf("dpproto: bad netlink length %d (have %d)", n, len(b))
		}
		m := nlmsg{
			typ:   nativeEndian.Uint16(b[4:]),
			flags: nativeEndian.Uint16(b[6:]),
			seq:   nativeEndian.Uint32(b[8:]),
			pid:   nativeEndian.Uint32(b[12:]),
			body:  b[nlmsgHdrLen:n],
		}
		if len(m.body) >= genlHdrLen && m.typ >= 0x10 {
			m.cmd, m.version = m.body[0], m.body[1]
		}
		out = append(out, m)
		b = b[min(align4(n), len(b)):]
	}
	return out, nil
}

// attrs is a parsed attribute list. Unknown attributes are kept and simply
// never asked for, which is what makes adding one backward compatible.
type attrs []attr

type attr struct {
	typ  uint16
	data []byte
}

func parseAttrs(b []byte) (attrs, error) {
	var out attrs
	for len(b) > 0 {
		if len(b) < nlattrHdrLen {
			return nil, errShort
		}
		n := int(nativeEndian.Uint16(b[0:]))
		if n < nlattrHdrLen || n > len(b) {
			return nil, fmt.Errorf("dpproto: bad attribute length %d", n)
		}
		out = append(out, attr{typ: nativeEndian.Uint16(b[2:]) & nlaTypeMask, data: b[nlattrHdrLen:n]})
		b = b[min(align4(n), len(b)):]
	}
	return out, nil
}

func (a attrs) get(typ uint16) ([]byte, bool) {
	for _, x := range a {
		if x.typ == typ {
			return x.data, true
		}
	}
	return nil, false
}

// The accessors return the zero value when the attribute is absent; want()
// is how a handler insists on one. Wrong-sized attributes count as absent.

func (a attrs) has(typ uint16) bool { _, ok := a.get(typ); return ok }

func (a attrs) u8(typ uint16) uint8 {
	if v, ok := a.get(typ); ok && len(v) >= 1 {
		return v[0]
	}
	return 0
}
func (a attrs) u16(typ uint16) uint16 {
	if v, ok := a.get(typ); ok && len(v) >= 2 {
		return nativeEndian.Uint16(v)
	}
	return 0
}
func (a attrs) u32(typ uint16) uint32 {
	if v, ok := a.get(typ); ok && len(v) >= 4 {
		return nativeEndian.Uint32(v)
	}
	return 0
}
func (a attrs) u64(typ uint16) uint64 {
	if v, ok := a.get(typ); ok && len(v) >= 8 {
		return nativeEndian.Uint64(v)
	}
	return 0
}
func (a attrs) str(typ uint16) string {
	v, _ := a.get(typ)
	return strings.TrimRight(string(v), "\x00")
}
func (a attrs) key(typ uint16) (k [32]byte) {
	if v, ok := a.get(typ); ok && len(v) == 32 {
		copy(k[:], v)
	}
	return
}

// bin returns a copy of a binary attribute (the receive buffer is reused).
func (a attrs) bin(typ uint16) []byte {
	v, _ := a.get(typ)
	return append([]byte(nil), v...)
}

// need reports which of the listed attributes are missing.
func (a attrs) need(types ...uint16) error {
	for _, t := range types {
		if !a.has(t) {
			return Errorf(CodeInvalid, "missing attribute %d", t)
		}
	}
	return nil
}

// ---- errors -----------------------------------------------------------------------

// errMessage builds an NLMSG_ERROR reply: an ack when e is nil, else the
// error's errno plus its text as an extended-ack message.
func errMessage(req nlmsg, e *Error) []byte {
	flags := uint16(0)
	if e != nil {
		flags = nlmFCapped | nlmFAckTLVs
	}
	m := newNL(nlmsgError, flags, req.seq, 0, 0, 0)
	m.b = m.b[:nlmsgHdrLen] // no genl header on NLMSG_ERROR
	var code int32
	if e != nil {
		code = -int32(e.Code)
	}
	m.b = nativeEndian.AppendUint32(m.b, uint32(code))
	// The original header, capped (we never echo the request body).
	orig := make([]byte, nlmsgHdrLen)
	nativeEndian.PutUint32(orig[0:], nlmsgHdrLen)
	nativeEndian.PutUint16(orig[4:], req.typ)
	nativeEndian.PutUint16(orig[6:], req.flags)
	nativeEndian.PutUint32(orig[8:], req.seq)
	nativeEndian.PutUint32(orig[12:], req.pid)
	m.b = append(m.b, orig...)
	if e != nil && e.Msg != "" {
		m.str(nlmsgerrAttrMsg, e.Msg)
	}
	return m.bytes()
}

// parseErrMessage decodes an NLMSG_ERROR into nil (ack) or an *Error.
func parseErrMessage(m nlmsg) error {
	if len(m.body) < 4 {
		return errShort
	}
	code := int32(nativeEndian.Uint32(m.body))
	if code == 0 {
		return nil
	}
	e := &Error{Code: uint16(-code)}
	rest := m.body[4:]
	// The original message: header only when capped, else all of it.
	if len(rest) >= nlmsgHdrLen {
		n := nlmsgHdrLen
		if m.flags&nlmFCapped == 0 {
			n = min(int(nativeEndian.Uint32(rest)), len(rest))
		}
		rest = rest[min(align4(n), len(rest)):]
	}
	if m.flags&nlmFAckTLVs != 0 {
		if as, err := parseAttrs(rest); err == nil {
			e.Msg = as.str(nlmsgerrAttrMsg)
		}
	}
	if e.Msg == "" {
		e.Msg = fmt.Sprintf("errno %d", -code)
	}
	return e
}

// ---- sockaddr ---------------------------------------------------------------------

// Endpoints travel as a struct sockaddr_in / sockaddr_in6 (what a kernel data
// plane has at hand), not as text.
const (
	afINET  = 2
	afINET6 = 10
)

func endpointToSockaddr(s string) ([]byte, error) {
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		return nil, fmt.Errorf("invalid endpoint %q (must be an ip:port literal): %v", s, err)
	}
	a := ap.Addr().Unmap()
	if a.Is4() {
		b := make([]byte, 16)
		nativeEndian.PutUint16(b[0:], afINET)
		binary.BigEndian.PutUint16(b[2:], ap.Port())
		copy(b[4:], a.AsSlice())
		return b, nil
	}
	b := make([]byte, 28)
	nativeEndian.PutUint16(b[0:], afINET6)
	binary.BigEndian.PutUint16(b[2:], ap.Port())
	copy(b[8:], a.AsSlice())
	if z := ap.Addr().Zone(); z != "" {
		return nil, fmt.Errorf("endpoint %q: zones are not supported", s)
	}
	return b, nil
}

func sockaddrToEndpoint(b []byte) (string, error) {
	if len(b) < 2 {
		return "", errShort
	}
	switch nativeEndian.Uint16(b) {
	case afINET:
		if len(b) < 8 {
			return "", errShort
		}
		return netip.AddrPortFrom(netip.AddrFrom4([4]byte(b[4:8])), binary.BigEndian.Uint16(b[2:])).String(), nil
	case afINET6:
		if len(b) < 24 {
			return "", errShort
		}
		return netip.AddrPortFrom(netip.AddrFrom16([16]byte(b[8:24])), binary.BigEndian.Uint16(b[2:])).String(), nil
	}
	return "", fmt.Errorf("unsupported address family %d", nativeEndian.Uint16(b))
}
