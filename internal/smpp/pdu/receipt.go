package pdu

import (
	"fmt"
	"strings"
	"time"
)

// Receipt is a delivery receipt as carried in deliver_sm short_message
// (SMPP 3.4 Appendix B format) and the receipt TLVs.
type Receipt struct {
	ID         string
	Sub        int
	Dlvrd      int
	SubmitDate time.Time
	DoneDate   time.Time
	Stat       string // DELIVRD, EXPIRED, DELETED, UNDELIV, ACCEPTD, UNKNOWN, REJECTD
	Err        string
	Text       string
}

const receiptDate = "0601021504"

// Format renders the receipt text.
func (r Receipt) Format() string {
	text := r.Text
	if len(text) > 20 {
		text = text[:20]
	}
	return fmt.Sprintf("id:%s sub:%03d dlvrd:%03d submit date:%s done date:%s stat:%s err:%s text:%s",
		r.ID, r.Sub, r.Dlvrd, r.SubmitDate.Format(receiptDate), r.DoneDate.Format(receiptDate),
		r.Stat, r.Err, text)
}

// MessageState values for the message_state TLV.
var messageState = map[string]byte{
	"ENROUTE": 1, "DELIVRD": 2, "EXPIRED": 3, "DELETED": 4,
	"UNDELIV": 5, "ACCEPTD": 6, "UNKNOWN": 7, "REJECTD": 8,
}

var stateName = map[byte]string{}

func init() {
	for k, v := range messageState {
		stateName[v] = k
	}
}

// ReceiptSM builds a deliver_sm body carrying r, for an original message
// sent from src to dst. TLVs are added for 3.4+ peers by the encoder.
func ReceiptSM(r Receipt, src, dst string) *SM {
	sm := &SM{
		Source:       dst, // a receipt travels back: from the handset to the sender
		Dest:         src,
		ESMClass:     ESMDeliveryReceipt,
		ShortMessage: []byte(r.Format()),
	}
	sm.TLVs.SetString(TagReceiptedMessageID, r.ID)
	if st, ok := messageState[r.Stat]; ok {
		sm.TLVs.Set(TagMessageState, []byte{st})
	}
	return sm
}

// ParseReceipt reads a receipt from a deliver_sm, preferring the TLVs for
// the message ID and state when present.
func ParseReceipt(sm *SM) (Receipt, error) {
	var r Receipt
	text := string(sm.ShortMessage)
	fields := parseReceiptText(text)
	r.ID = fields["id"]
	r.Stat = strings.ToUpper(fields["stat"])
	r.Err = fields["err"]
	r.Text = fields["text"]
	fmt.Sscanf(fields["sub"], "%d", &r.Sub)
	fmt.Sscanf(fields["dlvrd"], "%d", &r.Dlvrd)
	r.SubmitDate = parseReceiptDate(fields["submit date"])
	r.DoneDate = parseReceiptDate(fields["done date"])

	if id, ok := sm.TLVs.String(TagReceiptedMessageID); ok && id != "" {
		r.ID = id
	}
	if st, ok := sm.TLVs.Get(TagMessageState); ok && len(st) == 1 {
		if name, ok := stateName[st[0]]; ok {
			r.Stat = name
		}
	}
	if r.ID == "" {
		return r, fmt.Errorf("pdu: receipt without message id: %q", text)
	}
	if r.Stat == "" {
		r.Stat = "UNKNOWN"
	}
	return r, nil
}

// parseReceiptText splits "key:value key two:value ..." where keys are the
// known receipt keys (two of which contain a space).
func parseReceiptText(s string) map[string]string {
	keys := []string{"id", "sub", "dlvrd", "submit date", "done date", "stat", "err", "text"}
	out := map[string]string{}
	// ASCII-only lowering keeps every byte where it was, so offsets found
	// in lower are valid in s (strings.ToLower rewrites invalid UTF-8 into
	// longer sequences and the offsets would point elsewhere).
	lb := []byte(s)
	for i, c := range lb {
		if c >= 'A' && c <= 'Z' {
			lb[i] = c + 'a' - 'A'
		}
	}
	lower := string(lb)
	type pos struct {
		key        string
		start, val int
	}
	var found []pos
	for _, k := range keys {
		idx := strings.Index(lower, k+":")
		if idx < 0 {
			continue
		}
		found = append(found, pos{k, idx, idx + len(k) + 1})
	}
	for _, p := range found {
		end := len(s)
		for _, q := range found {
			if q.start > p.start && q.start < end {
				end = q.start
			}
		}
		out[p.key] = strings.TrimSpace(s[p.val:end])
	}
	return out
}

func parseReceiptDate(s string) time.Time {
	for _, layout := range []string{"060102150405", receiptDate} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
