// Package domain holds the core types shared across services.
package domain

import (
	"time"

	"github.com/shopspring/decimal"
)

// AccountRole places an account in the reseller tree.
type AccountRole string

const (
	RolePlatform AccountRole = "platform" // the operator itself (root)
	RoleReseller AccountRole = "reseller" // may own sub-accounts and set their credit limits
	RoleCustomer AccountRole = "customer" // sends traffic
)

// Account is a tenant. ParentID is empty for the platform root.
type Account struct {
	ID        string
	ParentID  string
	Role      AccountRole
	Name      string
	Status    string
	CreatedAt time.Time
}

// Wallet is an account's prepaid balance plus its credit limit.
// Available = Balance + CreditLimit - open holds. CreditLimit defaults to 0.
type Wallet struct {
	AccountID   string
	Currency    string // INR at launch
	Balance     decimal.Decimal
	CreditLimit decimal.Decimal
}

// Channel is how a message entered the platform.
type Channel string

const (
	ChannelSMPP Channel = "smpp"
	ChannelHTTP Channel = "http"
)

// MessageStatus is the lifecycle state of a message.
type MessageStatus string

const (
	StatusAccepted  MessageStatus = "accepted"  // in Kafka, balance held
	StatusRejected  MessageStatus = "rejected"  // refused by the platform after acceptance (no route, DLT)
	StatusSubmitted MessageStatus = "submitted" // vendor accepted it and returned message IDs
	StatusDelivered MessageStatus = "delivered"
	StatusFailed    MessageStatus = "failed"  // vendor refused it, or the handset was unreachable
	StatusExpired   MessageStatus = "expired" // no DLR before the vendor timeout (reconciliation)
)

// Final reports whether no further status change is expected.
func (s MessageStatus) Final() bool {
	switch s {
	case StatusRejected, StatusDelivered, StatusFailed, StatusExpired:
		return true
	}
	return false
}

// DLTCategory is the TRAI message category.
type DLTCategory string

const (
	DLTTransactional   DLTCategory = "transactional"
	DLTServiceImplicit DLTCategory = "service_implicit"
	DLTServiceExplicit DLTCategory = "service_explicit"
	DLTPromotional     DLTCategory = "promotional"
)

// DLTFields are the India DLT identifiers carried by every message.
type DLTFields struct {
	PEID       string `json:"pe_id,omitempty"`
	TemplateID string `json:"template_id,omitempty"` // detected when the customer omits it (phase 3)
	TMChain    string `json:"tm_chain,omitempty"`
}

// Charge is what one account pays for a message: the sender at its own
// price, and each reseller above it at the price the level above charges it.
type Charge struct {
	AccountID string          `json:"account_id"`
	HoldID    string          `json:"hold_id"`
	Amount    decimal.Decimal `json:"amount"`
}

// Route is the routing decision attached by the router.
type Route struct {
	VendorID string          `json:"vendor_id"`
	Cost     decimal.Decimal `json:"cost"` // vendor cost per segment
}

// Message is a submit as it travels through Kafka (sms.submit, then sms.route.<vendor>).
type Message struct {
	ID                 string          `json:"id"`
	AccountID          string          `json:"account_id"`
	ClientRef          string          `json:"client_ref,omitempty"`
	Channel            Channel         `json:"channel"`
	Sender             string          `json:"sender"`
	Destination        string          `json:"destination"` // E.164 without '+', e.g. 919812345678
	MCC                string          `json:"mcc"`
	MNC                string          `json:"mnc"`
	Text               string          `json:"text"`          // UTF-8; re-encoded by the connector
	UDH                []byte          `json:"udh,omitempty"` // customer-supplied concatenation header, sent as-is
	Segments           int             `json:"segments"`
	DLT                DLTFields       `json:"dlt"`
	RegisteredDelivery bool            `json:"registered_delivery"`
	HoldID             string          `json:"hold_id,omitempty"`
	Price              decimal.Decimal `json:"price"`             // total charged to the sending account
	Charges            []Charge        `json:"charges,omitempty"` // sender first, then resellers
	AcceptedAt         time.Time       `json:"accepted_at"`
	Route              *Route          `json:"route,omitempty"`
}

// StatusEvent is a state change emitted by the router or a connector
// (sms.status). The rater settles or releases holds from it and the DLR
// processor updates message state.
type StatusEvent struct {
	MessageID    string          `json:"message_id"`
	AccountID    string          `json:"account_id"`
	HoldID       string          `json:"hold_id,omitempty"`
	Price        decimal.Decimal `json:"price"`
	Charges      []Charge        `json:"charges,omitempty"`
	Status       MessageStatus   `json:"status"`
	VendorID     string          `json:"vendor_id,omitempty"`
	VendorMsgIDs []string        `json:"vendor_msg_ids,omitempty"`
	ErrorCode    string          `json:"error_code,omitempty"`
	At           time.Time       `json:"at"`
}

// DLR is a delivery receipt received from a vendor (sms.dlr).
type DLR struct {
	VendorID    string    `json:"vendor_id"`
	VendorMsgID string    `json:"vendor_msg_id"`
	Stat        string    `json:"stat"` // DELIVRD, UNDELIV, EXPIRED, REJECTD, ...
	ErrorCode   string    `json:"error_code,omitempty"`
	DoneAt      time.Time `json:"done_at"`
	ReceivedAt  time.Time `json:"received_at"`
	Attempt     int       `json:"attempt,omitempty"` // re-queue count when the ID mapping was not yet visible
}

// MO is a mobile-originated message received from a vendor (sms.mo).
type MO struct {
	VendorID   string    `json:"vendor_id"`
	From       string    `json:"from"`
	To         string    `json:"to"`
	Text       string    `json:"text"`
	ReceivedAt time.Time `json:"received_at"`
}
