// Package gsm7 converts between text and the GSM 03.38 default alphabet
// (unpacked, one septet per byte as SMPP carries it) and counts SMS segments.
package gsm7

import (
	"errors"
	"unicode/utf16"
)

// basic is the GSM 03.38 default alphabet, indexed by septet value.
var basic = [128]rune{
	'@', '£', '$', '¥', 'è', 'é', 'ù', 'ì', 'ò', 'Ç', '\n', 'Ø', 'ø', '\r', 'Å', 'å',
	'Δ', '_', 'Φ', 'Γ', 'Λ', 'Ω', 'Π', 'Ψ', 'Σ', 'Θ', 'Ξ', 0x1B, 'Æ', 'æ', 'ß', 'É',
	' ', '!', '"', '#', '¤', '%', '&', '\'', '(', ')', '*', '+', ',', '-', '.', '/',
	'0', '1', '2', '3', '4', '5', '6', '7', '8', '9', ':', ';', '<', '=', '>', '?',
	'¡', 'A', 'B', 'C', 'D', 'E', 'F', 'G', 'H', 'I', 'J', 'K', 'L', 'M', 'N', 'O',
	'P', 'Q', 'R', 'S', 'T', 'U', 'V', 'W', 'X', 'Y', 'Z', 'Ä', 'Ö', 'Ñ', 'Ü', '§',
	'¿', 'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h', 'i', 'j', 'k', 'l', 'm', 'n', 'o',
	'p', 'q', 'r', 's', 't', 'u', 'v', 'w', 'x', 'y', 'z', 'ä', 'ö', 'ñ', 'ü', 'à',
}

// ext is the extension table reached through the 0x1B escape.
var ext = map[byte]rune{
	0x0A: '\f', 0x14: '^', 0x28: '{', 0x29: '}', 0x2F: '\\',
	0x3C: '[', 0x3D: '~', 0x3E: ']', 0x40: '|', 0x65: '€',
}

const escape = 0x1B

var (
	basicRev = map[rune]byte{}
	extRev   = map[rune]byte{}
)

func init() {
	for i, r := range basic {
		if i != escape {
			basicRev[r] = byte(i)
		}
	}
	for b, r := range ext {
		extRev[r] = b
	}
}

// ErrNotGSM is returned when text has a character outside the GSM alphabet.
var ErrNotGSM = errors.New("gsm7: character not in GSM 03.38 alphabet")

// Septets returns how many septets s needs, and false if s is not GSM-encodable.
func Septets(s string) (int, bool) {
	n := 0
	for _, r := range s {
		if _, ok := basicRev[r]; ok {
			n++
		} else if _, ok := extRev[r]; ok {
			n += 2
		} else {
			return 0, false
		}
	}
	return n, true
}

// Encode converts s to unpacked GSM septets.
func Encode(s string) ([]byte, error) {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if b, ok := basicRev[r]; ok {
			out = append(out, b)
		} else if b, ok := extRev[r]; ok {
			out = append(out, escape, b)
		} else {
			return nil, ErrNotGSM
		}
	}
	return out, nil
}

// Decode converts unpacked GSM septets to text. Unknown escapes decode as a space.
func Decode(b []byte) string {
	out := make([]rune, 0, len(b))
	for i := 0; i < len(b); i++ {
		c := b[i] & 0x7F
		if c == escape && i+1 < len(b) {
			i++
			if r, ok := ext[b[i]&0x7F]; ok {
				out = append(out, r)
			} else {
				out = append(out, ' ')
			}
			continue
		}
		out = append(out, basic[c])
	}
	return string(out)
}

// EncodeUCS2 converts s to UCS-2/UTF-16 big-endian.
func EncodeUCS2(s string) []byte {
	u := utf16.Encode([]rune(s))
	out := make([]byte, 2*len(u))
	for i, v := range u {
		out[2*i] = byte(v >> 8)
		out[2*i+1] = byte(v)
	}
	return out
}

// DecodeUCS2 converts UTF-16 big-endian bytes to text.
func DecodeUCS2(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
	}
	return string(utf16.Decode(u))
}

// DecodeLatin1 converts ISO-8859-1 bytes to text.
func DecodeLatin1(b []byte) string {
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}

// Coding is how a message will be sent.
type Coding int

const (
	CodingGSM7 Coding = iota
	CodingUCS2
)

// Plan says how a text splits into SMS segments.
type Plan struct {
	Coding   Coding
	Units    int // septets for GSM7, UTF-16 code units for UCS2
	Segments int
}

// Segment limits: a single SMS, and each part of a concatenated SMS
// (6-byte UDH leaves 153 septets or 67 UCS-2 characters).
const (
	SingleGSM7 = 160
	PartGSM7   = 153
	SingleUCS2 = 70
	PartUCS2   = 67
)

// PlanFor chooses GSM7 when possible, otherwise UCS2, and counts segments.
func PlanFor(s string) Plan {
	if n, ok := Septets(s); ok {
		return Plan{Coding: CodingGSM7, Units: n, Segments: segments(n, SingleGSM7, PartGSM7)}
	}
	n := len(utf16.Encode([]rune(s)))
	return Plan{Coding: CodingUCS2, Units: n, Segments: segments(n, SingleUCS2, PartUCS2)}
}

func segments(n, single, part int) int {
	if n <= single {
		return 1
	}
	return (n + part - 1) / part
}

// Split encodes s for sending and splits it into parts that each fit one
// concatenated segment. An escape pair or surrogate pair is never split.
// A message that fits a single segment returns one part.
func Split(s string) (Coding, [][]byte) {
	plan := PlanFor(s)
	if plan.Coding == CodingGSM7 {
		enc, _ := Encode(s)
		if len(enc) <= SingleGSM7 {
			return CodingGSM7, [][]byte{enc}
		}
		var parts [][]byte
		for len(enc) > 0 {
			n := min(PartGSM7, len(enc))
			if n < len(enc) && enc[n-1] == escape {
				n--
			}
			parts = append(parts, enc[:n])
			enc = enc[n:]
		}
		return CodingGSM7, parts
	}
	enc := EncodeUCS2(s)
	if len(enc) <= 2*SingleUCS2 {
		return CodingUCS2, [][]byte{enc}
	}
	var parts [][]byte
	for len(enc) > 0 {
		n := min(2*PartUCS2, len(enc))
		if n < len(enc) {
			hi := uint16(enc[n-2])<<8 | uint16(enc[n-1])
			if hi >= 0xD800 && hi <= 0xDBFF { // high surrogate: keep the pair together
				n -= 2
			}
		}
		parts = append(parts, enc[:n])
		enc = enc[n:]
	}
	return CodingUCS2, parts
}
