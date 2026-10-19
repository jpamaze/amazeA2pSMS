// Package pdu encodes and decodes SMPP 3.3, 3.4 and 5.0 PDUs.
//
// Only the operations an SMS hub needs are typed: bind (all three kinds),
// unbind, submit_sm, deliver_sm, enquire_link and generic_nack, with their
// responses. TLVs are carried for 3.4 and 5.0 and dropped for 3.3.
package pdu

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Command IDs.
const (
	GenericNack         uint32 = 0x80000000
	BindReceiver        uint32 = 0x00000001
	BindReceiverResp    uint32 = 0x80000001
	BindTransmitter     uint32 = 0x00000002
	BindTransmitterResp uint32 = 0x80000002
	SubmitSM            uint32 = 0x00000004
	SubmitSMResp        uint32 = 0x80000004
	DeliverSM           uint32 = 0x00000005
	DeliverSMResp       uint32 = 0x80000005
	Unbind              uint32 = 0x00000006
	UnbindResp          uint32 = 0x80000006
	BindTransceiver     uint32 = 0x00000009
	BindTransceiverResp uint32 = 0x80000009
	EnquireLink         uint32 = 0x00000015
	EnquireLinkResp     uint32 = 0x80000015
)

// Command status codes used by this platform.
const (
	StatusOK            uint32 = 0x00000000
	StatusInvMsgLen     uint32 = 0x00000001
	StatusInvCmdLen     uint32 = 0x00000002
	StatusInvCmdID      uint32 = 0x00000003
	StatusInvBnd        uint32 = 0x00000004 // incorrect bind status for command
	StatusAlyBnd        uint32 = 0x00000005
	StatusSysErr        uint32 = 0x00000008
	StatusInvSrcAdr     uint32 = 0x0000000A
	StatusInvDstAdr     uint32 = 0x0000000B
	StatusBindFail      uint32 = 0x0000000D
	StatusInvPaswd      uint32 = 0x0000000E
	StatusInvSysID      uint32 = 0x0000000F
	StatusMsgQFul       uint32 = 0x00000014
	StatusSubmitFail    uint32 = 0x00000045
	StatusThrottled     uint32 = 0x00000058
	StatusInvDCS        uint32 = 0x00000104
	StatusInvOptParam   uint32 = 0x000000C3
	StatusDeliveryFail  uint32 = 0x000000FE
	StatusUnknownErr    uint32 = 0x000000FF
	StatusInvRegDlvFlag uint32 = 0x00000007 // registered delivery flag
)

// Interface versions.
const (
	V33 byte = 0x33
	V34 byte = 0x34
	V50 byte = 0x50
)

// IsResponse reports whether id is a response command.
func IsResponse(id uint32) bool { return id&0x80000000 != 0 }

// MaxPDULen bounds what we accept from a peer.
const MaxPDULen = 64 * 1024

const headerLen = 16

var (
	ErrTooLong  = errors.New("pdu: length exceeds limit")
	ErrTooShort = errors.New("pdu: length shorter than header")
	ErrBody     = errors.New("pdu: malformed body")
)

// PDU is one SMPP message. Body is nil for header-only commands.
type PDU struct {
	CommandID uint32
	Status    uint32
	Seq       uint32
	Body      Body
}

// Body is the typed body of a PDU.
type Body interface {
	encode(w *bytes.Buffer, version byte)
	decode(r *reader, version byte) error
}

// Bind is the body of all three bind requests.
type Bind struct {
	SystemID         string
	Password         string
	SystemType       string
	InterfaceVersion byte
	AddrTON, AddrNPI byte
	AddressRange     string
}

// BindResp is the body of all three bind responses.
type BindResp struct {
	SystemID string
	TLVs     TLVs
}

// SM is the body shared by submit_sm and deliver_sm.
type SM struct {
	ServiceType          string
	SourceTON, SourceNPI byte
	Source               string
	DestTON, DestNPI     byte
	Dest                 string
	ESMClass             byte
	ProtocolID           byte
	PriorityFlag         byte
	ScheduleDeliveryTime string
	ValidityPeriod       string
	RegisteredDelivery   byte
	ReplaceIfPresent     byte
	DataCoding           byte
	SMDefaultMsgID       byte
	ShortMessage         []byte
	TLVs                 TLVs
}

// SMResp is the body of submit_sm_resp and deliver_sm_resp.
type SMResp struct {
	MessageID string
}

// ESM class bits.
const (
	ESMDeliveryReceipt byte = 0x04 // message type: SMSC delivery receipt
	ESMUDHI            byte = 0x40 // short_message starts with a UDH
	ESMTypeMask        byte = 0x3C
)

// IsReceipt reports whether a deliver_sm is a delivery receipt.
func (s *SM) IsReceipt() bool { return s.ESMClass&ESMTypeMask == ESMDeliveryReceipt }

// HasUDH reports whether short_message starts with a user data header.
func (s *SM) HasUDH() bool { return s.ESMClass&ESMUDHI != 0 }

// Payload returns message_payload when present, else short_message.
func (s *SM) Payload() []byte {
	if v, ok := s.TLVs.Get(TagMessagePayload); ok {
		return v
	}
	return s.ShortMessage
}

func (b *Bind) encode(w *bytes.Buffer, _ byte) {
	cstr(w, b.SystemID)
	cstr(w, b.Password)
	cstr(w, b.SystemType)
	w.WriteByte(b.InterfaceVersion)
	w.WriteByte(b.AddrTON)
	w.WriteByte(b.AddrNPI)
	cstr(w, b.AddressRange)
}

func (b *Bind) decode(r *reader, _ byte) error {
	b.SystemID = r.cstr(16)
	b.Password = r.cstr(9)
	b.SystemType = r.cstr(13)
	b.InterfaceVersion = r.byte()
	b.AddrTON = r.byte()
	b.AddrNPI = r.byte()
	b.AddressRange = r.cstr(41)
	return r.err
}

func (b *BindResp) encode(w *bytes.Buffer, v byte) {
	cstr(w, b.SystemID)
	if v >= V34 {
		b.TLVs.encode(w)
	}
}

func (b *BindResp) decode(r *reader, _ byte) error {
	b.SystemID = r.cstr(16)
	if r.err == nil && r.remaining() > 0 {
		b.TLVs = r.tlvs()
	}
	return r.err
}

func (s *SM) encode(w *bytes.Buffer, v byte) {
	cstr(w, s.ServiceType)
	w.WriteByte(s.SourceTON)
	w.WriteByte(s.SourceNPI)
	cstr(w, s.Source)
	w.WriteByte(s.DestTON)
	w.WriteByte(s.DestNPI)
	cstr(w, s.Dest)
	w.WriteByte(s.ESMClass)
	w.WriteByte(s.ProtocolID)
	w.WriteByte(s.PriorityFlag)
	cstr(w, s.ScheduleDeliveryTime)
	cstr(w, s.ValidityPeriod)
	w.WriteByte(s.RegisteredDelivery)
	w.WriteByte(s.ReplaceIfPresent)
	w.WriteByte(s.DataCoding)
	w.WriteByte(s.SMDefaultMsgID)
	msg, tlvs := s.ShortMessage, s.TLVs
	if len(msg) > 254 {
		// Too long for short_message: send it as message_payload (3.4+);
		// 3.3 peers must be given pre-split messages by the caller.
		tlvs = append(append(TLVs(nil), tlvs...), TLV{Tag: TagMessagePayload, Value: msg})
		msg = nil
	}
	w.WriteByte(byte(len(msg)))
	w.Write(msg)
	if v >= V34 {
		tlvs.encode(w)
	}
}

func (s *SM) decode(r *reader, _ byte) error {
	s.ServiceType = r.cstr(6)
	s.SourceTON = r.byte()
	s.SourceNPI = r.byte()
	s.Source = r.cstr(21)
	s.DestTON = r.byte()
	s.DestNPI = r.byte()
	s.Dest = r.cstr(21)
	s.ESMClass = r.byte()
	s.ProtocolID = r.byte()
	s.PriorityFlag = r.byte()
	s.ScheduleDeliveryTime = r.cstr(17)
	s.ValidityPeriod = r.cstr(17)
	s.RegisteredDelivery = r.byte()
	s.ReplaceIfPresent = r.byte()
	s.DataCoding = r.byte()
	s.SMDefaultMsgID = r.byte()
	n := int(r.byte())
	s.ShortMessage = r.bytes(n)
	if r.err == nil && r.remaining() > 0 {
		s.TLVs = r.tlvs()
	}
	return r.err
}

func (s *SMResp) encode(w *bytes.Buffer, _ byte) { cstr(w, s.MessageID) }

func (s *SMResp) decode(r *reader, _ byte) error {
	// A failed submit_sm_resp may have no body at all.
	if r.remaining() > 0 {
		s.MessageID = r.cstr(65)
	}
	return r.err
}

// newBody returns an empty body for id, or nil for header-only commands.
func newBody(id uint32) Body {
	switch id {
	case BindReceiver, BindTransmitter, BindTransceiver:
		return &Bind{}
	case BindReceiverResp, BindTransmitterResp, BindTransceiverResp:
		return &BindResp{}
	case SubmitSM, DeliverSM:
		return &SM{}
	case SubmitSMResp, DeliverSMResp:
		return &SMResp{}
	}
	return nil
}

// Known reports whether id is a command this package understands.
func Known(id uint32) bool {
	switch id {
	case GenericNack, Unbind, UnbindResp, EnquireLink, EnquireLinkResp:
		return true
	}
	return newBody(id) != nil
}

// Encode serialises p. version controls whether TLVs are written.
func Encode(p *PDU, version byte) []byte {
	var w bytes.Buffer
	w.Write(make([]byte, headerLen))
	if p.Body != nil {
		p.Body.encode(&w, version)
	}
	b := w.Bytes()
	binary.BigEndian.PutUint32(b[0:], uint32(len(b)))
	binary.BigEndian.PutUint32(b[4:], p.CommandID)
	binary.BigEndian.PutUint32(b[8:], p.Status)
	binary.BigEndian.PutUint32(b[12:], p.Seq)
	return b
}

// Read reads one PDU from r. A PDU with an unknown command ID is returned
// with a nil Body so the caller can answer generic_nack.
func Read(r io.Reader, version byte) (*PDU, error) {
	var hdr [headerLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[0:])
	if n < headerLen {
		return nil, ErrTooShort
	}
	if n > MaxPDULen {
		return nil, ErrTooLong
	}
	body := make([]byte, n-headerLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	p := &PDU{
		CommandID: binary.BigEndian.Uint32(hdr[4:]),
		Status:    binary.BigEndian.Uint32(hdr[8:]),
		Seq:       binary.BigEndian.Uint32(hdr[12:]),
	}
	b := newBody(p.CommandID)
	if b == nil {
		return p, nil
	}
	// Error responses may carry an empty or truncated body; keep the header.
	if IsResponse(p.CommandID) && p.Status != StatusOK && len(body) == 0 {
		return p, nil
	}
	rd := &reader{b: body}
	if err := b.decode(rd, version); err != nil {
		return p, fmt.Errorf("%w: command 0x%08x: %v", ErrBody, p.CommandID, err)
	}
	p.Body = b
	return p, nil
}

// Decode parses one complete PDU from b.
func Decode(b []byte, version byte) (*PDU, error) {
	return Read(bytes.NewReader(b), version)
}

func cstr(w *bytes.Buffer, s string) {
	w.WriteString(s)
	w.WriteByte(0)
}

type reader struct {
	b   []byte
	off int
	err error
}

func (r *reader) remaining() int { return len(r.b) - r.off }

func (r *reader) fail(msg string) {
	if r.err == nil {
		r.err = errors.New(msg)
	}
}

func (r *reader) byte() byte {
	if r.err != nil {
		return 0
	}
	if r.off >= len(r.b) {
		r.fail("unexpected end of body")
		return 0
	}
	c := r.b[r.off]
	r.off++
	return c
}

func (r *reader) bytes(n int) []byte {
	if r.err != nil {
		return nil
	}
	if r.off+n > len(r.b) {
		r.fail("short_message longer than body")
		return nil
	}
	out := make([]byte, n)
	copy(out, r.b[r.off:r.off+n])
	r.off += n
	return out
}

// cstr reads a NUL-terminated string of at most max bytes including the NUL.
func (r *reader) cstr(max int) string {
	if r.err != nil {
		return ""
	}
	i := bytes.IndexByte(r.b[r.off:], 0)
	if i < 0 {
		r.fail("unterminated C-octet string")
		return ""
	}
	if i+1 > max {
		r.fail(fmt.Sprintf("C-octet string longer than %d", max-1))
		return ""
	}
	s := string(r.b[r.off : r.off+i])
	r.off += i + 1
	return s
}

func (r *reader) tlvs() TLVs {
	var out TLVs
	for r.err == nil && r.remaining() > 0 {
		if r.remaining() < 4 {
			r.fail("truncated TLV header")
			return out
		}
		tag := binary.BigEndian.Uint16(r.b[r.off:])
		n := int(binary.BigEndian.Uint16(r.b[r.off+2:]))
		r.off += 4
		v := r.bytes(n)
		out = append(out, TLV{Tag: tag, Value: v})
	}
	return out
}
