package pdu

import (
	"bytes"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBindWireFormat(t *testing.T) {
	p := &PDU{CommandID: BindTransceiver, Seq: 1, Body: &Bind{
		SystemID: "acme", Password: "secret", InterfaceVersion: V34,
	}}
	got := hex.EncodeToString(Encode(p, V34))
	// length 0x21=33, id 9, status 0, seq 1, "acme\0" "secret\0" "\0" 0x34 0 0 "\0"
	want := "00000021" + "00000009" + "00000000" + "00000001" +
		"61636d6500" + "73656372657400" + "00" + "34" + "00" + "00" + "00"
	if got != want {
		t.Fatalf("bind encoding\n got %s\nwant %s", got, want)
	}
	back, err := Decode(Encode(p, V34), V34)
	if err != nil || !reflect.DeepEqual(back, p) {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}

func TestSubmitRoundTripWithTLVs(t *testing.T) {
	sm := &SM{
		SourceTON: 5, Source: "AMZTST", DestTON: 1, DestNPI: 1, Dest: "919812345678",
		RegisteredDelivery: 1, DataCoding: 8, ShortMessage: []byte{0x09, 0x15},
	}
	sm.TLVs.SetString(0x1400, "1101234567890123456")
	sm.TLVs.SetString(0x1401, "1107160000000012345")
	p := &PDU{CommandID: SubmitSM, Seq: 42, Body: sm}

	back, err := Decode(Encode(p, V34), V34)
	if err != nil {
		t.Fatal(err)
	}
	got := back.Body.(*SM)
	if pe, _ := got.TLVs.String(0x1400); pe != "1101234567890123456" {
		t.Fatalf("PE ID TLV lost: %q", pe)
	}
	if !reflect.DeepEqual(got, sm) {
		t.Fatalf("round trip\n got %+v\nwant %+v", got, sm)
	}

	// SMPP 3.3 drops TLVs.
	back33, err := Decode(Encode(p, V33), V33)
	if err != nil || len(back33.Body.(*SM).TLVs) != 0 {
		t.Fatalf("3.3 must not carry TLVs: %+v %v", back33.Body, err)
	}
}

func TestLongMessageMovesToPayload(t *testing.T) {
	long := bytes.Repeat([]byte("x"), 400)
	p := &PDU{CommandID: SubmitSM, Seq: 1, Body: &SM{Dest: "1", ShortMessage: long}}
	back, err := Decode(Encode(p, V34), V34)
	if err != nil {
		t.Fatal(err)
	}
	sm := back.Body.(*SM)
	if len(sm.ShortMessage) != 0 || !bytes.Equal(sm.Payload(), long) {
		t.Fatalf("payload: short %d, payload %d", len(sm.ShortMessage), len(sm.Payload()))
	}
}

func TestMalformedInputs(t *testing.T) {
	ok := Encode(&PDU{CommandID: SubmitSM, Seq: 1, Body: &SM{Dest: "1", ShortMessage: []byte("hi")}}, V34)

	cases := map[string][]byte{
		"short length": {0, 0, 0, 8, 0, 0, 0, 4, 0, 0, 0, 0, 0, 0, 0, 1},
		"huge length":  {0x7f, 0, 0, 0, 0, 0, 0, 4, 0, 0, 0, 0, 0, 0, 0, 1},
	}
	for name, b := range cases {
		if _, err := Decode(b, V34); err == nil {
			t.Errorf("%s: want error", name)
		}
	}

	// sm_length claiming more than the body holds.
	bad := append([]byte(nil), ok...)
	idx := bytes.Index(bad, []byte("hi")) - 1
	bad[idx] = 200
	if _, err := Decode(bad, V34); !errors.Is(err, ErrBody) {
		t.Errorf("sm_length overflow: want ErrBody, got %v", err)
	}

	// Unterminated system_id.
	bind := Encode(&PDU{CommandID: BindTransmitter, Seq: 1, Body: &Bind{SystemID: strings.Repeat("a", 30)}}, V34)
	if _, err := Decode(bind, V34); !errors.Is(err, ErrBody) {
		t.Errorf("over-long system_id: want ErrBody, got %v", err)
	}

	// Unknown command: header only, no error, so the caller can generic_nack.
	unk := Encode(&PDU{CommandID: 0x00000103, Seq: 9}, V34)
	p, err := Decode(unk, V34)
	if err != nil || p.Body != nil || Known(p.CommandID) {
		t.Errorf("unknown command: %+v %v", p, err)
	}

	// Failed submit_sm_resp with no body.
	fail := Encode(&PDU{CommandID: SubmitSMResp, Status: StatusThrottled, Seq: 3}, V34)
	p, err = Decode(fail, V34)
	if err != nil || p.Status != StatusThrottled {
		t.Errorf("empty error resp: %+v %v", p, err)
	}
}

func TestReceiptFormatAndParse(t *testing.T) {
	when := time.Date(2026, 10, 19, 14, 30, 0, 0, time.UTC)
	r := Receipt{ID: "abc-123", Sub: 1, Dlvrd: 1, SubmitDate: when, DoneDate: when.Add(time.Minute),
		Stat: "DELIVRD", Err: "000", Text: "Your OTP is 123456 stat:x"}
	sm := ReceiptSM(r, "AMZTST", "919812345678")
	if !sm.IsReceipt() || sm.Dest != "AMZTST" {
		t.Fatalf("receipt PDU: %+v", sm)
	}
	got, err := ParseReceipt(sm)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "abc-123" || got.Stat != "DELIVRD" || got.Err != "000" || !got.DoneDate.Equal(when.Add(time.Minute)) {
		t.Fatalf("parsed: %+v", got)
	}

	// Vendor that sends text only, upper-case keys, seconds in dates.
	vendor := &SM{ESMClass: ESMDeliveryReceipt, ShortMessage: []byte(
		"id:0A1B2C sub:001 dlvrd:000 submit date:261019143000 done date:261019143105 stat:UNDELIV err:034 Text:hello")}
	got, err = ParseReceipt(vendor)
	if err != nil || got.ID != "0A1B2C" || got.Stat != "UNDELIV" || got.Err != "034" || got.DoneDate.Second() != 5 {
		t.Fatalf("vendor receipt: %+v %v", got, err)
	}

	// TLVs win over text.
	vendor.TLVs.SetString(TagReceiptedMessageID, "TLVID")
	vendor.TLVs.Set(TagMessageState, []byte{2})
	got, _ = ParseReceipt(vendor)
	if got.ID != "TLVID" || got.Stat != "DELIVRD" {
		t.Fatalf("tlv precedence: %+v", got)
	}
}

func TestParseTag(t *testing.T) {
	for in, want := range map[string]uint16{"0x1400": 0x1400, "5121": 5121, " 0X1402 ": 0x1402} {
		got, err := ParseTag(in)
		if err != nil || got != want {
			t.Errorf("ParseTag(%q) = %x, %v", in, got, err)
		}
	}
	if _, err := ParseTag("0xZZ"); err == nil {
		t.Error("bad tag must fail")
	}
}
