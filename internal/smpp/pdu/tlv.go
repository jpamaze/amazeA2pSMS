package pdu

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

// Standard TLV tags used by the platform.
const (
	TagSCInterfaceVersion   uint16 = 0x0210
	TagReceiptedMessageID   uint16 = 0x001E
	TagNetworkErrorCode     uint16 = 0x0423
	TagMessagePayload       uint16 = 0x0424
	TagMessageState         uint16 = 0x0427
	TagSarMsgRefNum         uint16 = 0x020C
	TagSarTotalSegments     uint16 = 0x020E
	TagSarSegmentSeqnum     uint16 = 0x020F
	TagUserMessageReference uint16 = 0x0204
)

// TLV is one optional parameter.
type TLV struct {
	Tag   uint16
	Value []byte
}

// TLVs is an ordered list of optional parameters.
type TLVs []TLV

// Get returns the first value for tag.
func (t TLVs) Get(tag uint16) ([]byte, bool) {
	for _, v := range t {
		if v.Tag == tag {
			return v.Value, true
		}
	}
	return nil, false
}

// String returns a C-string TLV value without its trailing NUL.
func (t TLVs) String(tag uint16) (string, bool) {
	v, ok := t.Get(tag)
	if !ok {
		return "", false
	}
	return string(bytes.TrimRight(v, "\x00")), true
}

// Set replaces or appends tag.
func (t *TLVs) Set(tag uint16, value []byte) {
	for i := range *t {
		if (*t)[i].Tag == tag {
			(*t)[i].Value = value
			return
		}
	}
	*t = append(*t, TLV{Tag: tag, Value: value})
}

// SetString stores s as a NUL-terminated C-string.
func (t *TLVs) SetString(tag uint16, s string) {
	t.Set(tag, append([]byte(s), 0))
}

func (t TLVs) encode(w *bytes.Buffer) {
	var hdr [4]byte
	for _, v := range t {
		binary.BigEndian.PutUint16(hdr[0:], v.Tag)
		binary.BigEndian.PutUint16(hdr[2:], uint16(len(v.Value)))
		w.Write(hdr[:])
		w.Write(v.Value)
	}
}

// ParseTag parses a tag written as "0x1400" or "5120".
func ParseTag(s string) (uint16, error) {
	s = strings.TrimSpace(s)
	base := 10
	if strings.HasPrefix(strings.ToLower(s), "0x") {
		s, base = s[2:], 16
	}
	v, err := strconv.ParseUint(s, base, 16)
	if err != nil {
		return 0, fmt.Errorf("pdu: bad TLV tag %q: %w", s, err)
	}
	return uint16(v), nil
}
