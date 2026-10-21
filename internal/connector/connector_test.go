package connector

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/jpamaze/amazeA2pSMS/internal/domain"
	"github.com/jpamaze/amazeA2pSMS/internal/smpp/gsm7"
	"github.com/jpamaze/amazeA2pSMS/internal/smpp/pdu"
	"github.com/jpamaze/amazeA2pSMS/internal/smpp/session"
)

func newConn() *SMPPTransport {
	return &SMPPTransport{TLVs: TLVProfile{PEID: 0x1400, TemplateID: 0x1401, TMChain: 0x1402}}
}

func TestBuildSingleWithDLT(t *testing.T) {
	m := &domain.Message{Sender: "AMZTST", Destination: "919812345678", Text: "OTP 123",
		DLT: domain.DLTFields{PEID: "PE1", TemplateID: "T1"}}
	parts := newConn().build(m)
	if len(parts) != 1 {
		t.Fatalf("parts: %d", len(parts))
	}
	sm := parts[0]
	if sm.SourceTON != 5 || sm.DestTON != 1 || sm.DataCoding != 0 || sm.RegisteredDelivery != 1 {
		t.Fatalf("addressing: %+v", sm)
	}
	if pe, _ := sm.TLVs.String(0x1400); pe != "PE1" {
		t.Fatalf("PE ID TLV: %q", pe)
	}
	if _, ok := sm.TLVs.Get(0x1402); ok {
		t.Fatal("empty TM chain must not be sent")
	}
	if gsm7.Decode(sm.ShortMessage) != "OTP 123" {
		t.Fatalf("text: %q", sm.ShortMessage)
	}

	m.Sender = "919900001111"
	if sm := newConn().build(m)[0]; sm.SourceTON != 1 || sm.SourceNPI != 1 {
		t.Fatalf("numeric sender TON/NPI: %d/%d", sm.SourceTON, sm.SourceNPI)
	}
}

func TestBuildConcatenatedUCS2(t *testing.T) {
	text := strings.Repeat("नमस्ते ", 20) // 140 UTF-16 units -> 3 parts of 67
	m := &domain.Message{Sender: "AMZTST", Destination: "919812345678", Text: text,
		DLT: domain.DLTFields{PEID: "PE1"}}
	parts := newConn().build(m)
	if len(parts) != 3 {
		t.Fatalf("parts: %d", len(parts))
	}
	var joined []byte
	ref := parts[0].ShortMessage[3]
	for i, sm := range parts {
		udh := sm.ShortMessage[:6]
		if sm.ESMClass&pdu.ESMUDHI == 0 || sm.DataCoding != 0x08 {
			t.Fatalf("part %d: esm %x dc %x", i, sm.ESMClass, sm.DataCoding)
		}
		if !bytes.Equal(udh, []byte{5, 0, 3, ref, 3, byte(i + 1)}) {
			t.Fatalf("part %d UDH: % x", i, udh)
		}
		if pe, _ := sm.TLVs.String(0x1400); pe != "PE1" {
			t.Fatalf("part %d lost DLT TLV", i)
		}
		joined = append(joined, sm.ShortMessage[6:]...)
	}
	if gsm7.DecodeUCS2(joined) != text {
		t.Fatal("parts do not reassemble to the original text")
	}
	// Parts must not share a TLV slice (mutation of one would leak).
	parts[0].TLVs.SetString(0x1400, "X")
	if pe, _ := parts[1].TLVs.String(0x1400); pe != "PE1" {
		t.Fatal("parts share TLV storage")
	}
}

func TestBuildCustomerUDHPassthrough(t *testing.T) {
	udh := []byte{5, 0, 3, 9, 2, 1}
	m := &domain.Message{Sender: "AMZTST", Destination: "919812345678", Text: "part one", UDH: udh}
	parts := newConn().build(m)
	if len(parts) != 1 || !bytes.HasPrefix(parts[0].ShortMessage, udh) || parts[0].ESMClass&pdu.ESMUDHI == 0 {
		t.Fatalf("customer UDH not passed through: %+v", parts)
	}
}

func TestTemporary(t *testing.T) {
	cases := map[error]bool{
		&session.StatusError{Status: pdu.StatusThrottled}:  true,
		&session.StatusError{Status: pdu.StatusMsgQFul}:    true,
		&session.StatusError{Status: pdu.StatusInvDstAdr}:  false,
		&session.StatusError{Status: pdu.StatusSubmitFail}: false,
		session.ErrTimeout:  true,
		session.ErrClosed:   true,
		errors.New("other"): false,
	}
	for err, want := range cases {
		if got := temporary(err); got != want {
			t.Errorf("temporary(%v) = %v", err, got)
		}
	}
	if got := errorCode(&session.StatusError{Status: pdu.StatusInvDstAdr}); got != "SMPP_0000000B" {
		t.Errorf("errorCode: %s", got)
	}
}
