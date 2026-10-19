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
	StatusRejected  MessageStatus = "rejected"  // failed validation, DLT or balance
	StatusRouted    MessageStatus = "routed"    // vendor chosen
	StatusSubmitted MessageStatus = "submitted" // vendor returned a message ID
	StatusDelivered MessageStatus = "delivered"
	StatusFailed    MessageStatus = "failed"
	StatusExpired   MessageStatus = "expired" // no DLR before the vendor timeout (reconciliation)
)

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
	PEID       string `json:"pe_id"`
	TemplateID string `json:"template_id,omitempty"` // detected when the customer omits it
	TMChain    string `json:"tm_chain,omitempty"`
}

// Message is a submit as it travels through Kafka.
type Message struct {
	ID          string          `json:"id"`
	AccountID   string          `json:"account_id"`
	ClientRef   string          `json:"client_ref"`
	Channel     Channel         `json:"channel"`
	Sender      string          `json:"sender"`
	Destination string          `json:"destination"` // E.164
	Body        string          `json:"body"`
	DataCoding  uint8           `json:"data_coding"`
	Segments    int             `json:"segments"`
	DLT         DLTFields       `json:"dlt"`
	HoldID      string          `json:"hold_id"`
	Cost        decimal.Decimal `json:"cost"`
	AcceptedAt  time.Time       `json:"accepted_at"`
}

// DLR is a delivery report received from a vendor.
type DLR struct {
	VendorID    string        `json:"vendor_id"`
	VendorMsgID string        `json:"vendor_msg_id"`
	Status      MessageStatus `json:"status"`
	ErrorCode   string        `json:"error_code,omitempty"`
	DoneAt      time.Time     `json:"done_at"`
	ReceivedAt  time.Time     `json:"received_at"`
}
