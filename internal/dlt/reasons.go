// Package dlt checks messages to Indian numbers against the sender's DLT
// registrations (TRAI TCCCPR): principal entity, header, content template,
// category, promotional hours, whitelisted links and numbers, and the
// telemarketer chain. A message that operator scrubbing would reject is
// refused at submit, with a reason the customer can act on. The same
// reasons describe operator scrubbing failures reported in receipts.
package dlt

import (
	"errors"
	"fmt"
)

// Reason is why a message fails DLT. It is the HTTP error code, the
// metrics label and the status API's error_code.
type Reason string

// Reasons, with the code customers see in receipts (err:6xx).
const (
	ReasonEntity     Reason = "dlt_entity"      // PE ID not registered for the account, or suspended
	ReasonHeader     Reason = "dlt_header"      // sender not a registered header, or suspended
	ReasonTemplate   Reason = "dlt_template"    // template not registered for the header, suspended, or text does not match
	ReasonCategory   Reason = "dlt_category"    // template category not allowed with the header type
	ReasonPromoHours Reason = "dlt_promo_hours" // promotional outside 09:00-21:00 IST
	ReasonConsent    Reason = "dlt_consent"     // service explicit without consent
	ReasonChain      Reason = "dlt_tm_chain"    // no or invalid telemarketer chain
	ReasonURL        Reason = "dlt_url"         // link or APK not whitelisted on the template
	ReasonCallback   Reason = "dlt_callback"    // callback number not whitelisted
	ReasonDND        Reason = "dlt_dnd"         // recipient on the DND/NCPR list (operator only)
	ReasonOther      Reason = "dlt_other"       // operator DLT failure without detail
)

// Info is a reason's customer-facing details.
type Info struct {
	Code       string // receipt err: field
	SMPPStatus uint32 // command_status on submit_sm_resp (vendor-specific range)
	Text       string
}

// Catalog lists every reason.
var Catalog = map[Reason]Info{
	ReasonEntity:     {"601", 0x00000410, "principal entity (PE ID) not registered for this account, or suspended"},
	ReasonHeader:     {"602", 0x00000411, "sender is not a registered DLT header, or it is suspended"},
	ReasonTemplate:   {"603", 0x00000412, "message does not match a registered, active template for this header"},
	ReasonCategory:   {"604", 0x00000413, "template category is not allowed with this header type"},
	ReasonPromoHours: {"605", 0x00000414, "promotional messages are allowed only 09:00-21:00 IST"},
	ReasonConsent:    {"606", 0x00000415, "service explicit template without a consent template"},
	ReasonChain:      {"607", 0x00000416, "no valid telemarketer (PE-TM) chain"},
	ReasonURL:        {"608", 0x00000417, "link or APK not whitelisted on the template"},
	ReasonCallback:   {"609", 0x00000418, "callback number not whitelisted on the template"},
	ReasonDND:        {"610", 0x00000419, "recipient is on the DND (NCPR) list for this category"},
	ReasonOther:      {"600", 0x0000041F, "rejected by operator DLT scrubbing"},
}

// ErrDLT matches any *Error with errors.Is.
var ErrDLT = errors.New("dlt check failed")

// Error is a DLT refusal.
type Error struct {
	Reason Reason
	Detail string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Reason, e.Detail) }

// Is makes errors.Is(err, ErrDLT) true.
func (e *Error) Is(target error) bool { return target == ErrDLT }

// SMPPStatus is the submit_sm_resp status for the refusal.
func (e *Error) SMPPStatus() uint32 { return Catalog[e.Reason].SMPPStatus }

func fail(r Reason, format string, a ...any) *Error {
	return &Error{Reason: r, Detail: fmt.Sprintf(format, a...)}
}

// AsError returns the *Error inside err, if any.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}
