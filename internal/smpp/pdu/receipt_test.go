package pdu

import (
	"strings"
	"testing"
)

// A receipt with invalid UTF-8 (hostile or broken vendor) must not panic.
func TestReceiptWithInvalidUTF8(t *testing.T) {
	for _, text := range []string{
		strings.Repeat("\xff", 40) + "stat:X",
		"id:ABC \xff\xfe sub:001 dlvrd:001 submit date:2610211200 done date:2610211201 stat:DELIVRD err:000 text:\xc3",
		"ID:\xe2\x82 STAT:DELIVRD",
	} {
		_, _ = ParseReceipt(&SM{ESMClass: 0x04, ShortMessage: []byte(text)}) // must not panic
	}
	r, err := ParseReceipt(&SM{ESMClass: 0x04, ShortMessage: []byte("id:A1B2 \xff\xfe sub:001 dlvrd:001 stat:DELIVRD err:000")})
	if err != nil || !strings.HasPrefix(r.ID, "A1B2") || r.Stat != "DELIVRD" {
		t.Fatalf("parsed %+v %v", r, err)
	}
}
