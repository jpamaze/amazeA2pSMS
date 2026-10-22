package ingest

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckPart(t *testing.T) {
	concat8 := []byte{5, 0x00, 3, 0x42, 2, 1}        // ref 0x42, part 1 of 2
	concat16 := []byte{6, 0x08, 4, 0x12, 0x34, 3, 3} // 16-bit ref, part 3 of 3
	for _, c := range []struct {
		name string
		udh  []byte
		text string
		want error
	}{
		{"8-bit concat, 153 GSM chars", concat8, strings.Repeat("a", 153), nil},
		{"16-bit concat, 152 GSM chars", concat16, strings.Repeat("a", 152), nil},
		{"GSM part too long", concat8, strings.Repeat("a", 154), ErrTooLong},
		{"UCS-2 part, 67 chars", concat8, strings.Repeat("अ", 67), nil},
		{"UCS-2 part too long", concat8, strings.Repeat("अ", 68), ErrTooLong},
		{"a whole message hidden behind a UDH", concat8, strings.Repeat("phish ", 10000), ErrTooLong},
		{"one byte UDH", []byte{0}, "x", ErrBadUDH},
		{"length byte lies", []byte{9, 0x00, 3, 1, 2, 1}, "x", ErrBadUDH},
		{"port addressing (WAP push)", []byte{6, 0x05, 4, 0x0B, 0x84, 0x23, 0xF0}, "x", ErrBadUDH},
		{"concat plus another element", []byte{9, 0x00, 3, 1, 2, 1, 0x05, 2, 1, 1}, "x", ErrBadUDH},
		{"part 0", []byte{5, 0x00, 3, 1, 2, 0}, "x", ErrBadUDH},
		{"part 3 of 2", []byte{5, 0x00, 3, 1, 2, 3}, "x", ErrBadUDH},
		{"more parts than allowed", []byte{5, 0x00, 3, 1, 200, 1}, "x", ErrBadUDH},
		{"element runs past the end", []byte{3, 0x00, 3, 1}, "x", ErrBadUDH},
	} {
		if err := checkPart(c.udh, c.text); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
}
