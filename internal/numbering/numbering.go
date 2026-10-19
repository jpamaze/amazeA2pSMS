// Package numbering validates and normalises destination numbers and sender IDs.
//
// Launch scope is India only: every destination must be an Indian mobile
// number. Network (MNC) resolution from HLR/MNP comes in phase 3; until then
// all Indian mobiles map to MCC 404 with MNC "*".
package numbering

import (
	"errors"
	"strings"
)

var (
	ErrInvalidDestination = errors.New("numbering: destination is not a valid Indian mobile number")
	ErrInvalidSender      = errors.New("numbering: invalid sender ID")
)

// Destination is a normalised mobile number.
type Destination struct {
	E164 string // without '+', e.g. 919812345678
	MCC  string
	MNC  string // "*" until HLR/MNP lookup
}

// NormalizeIndia accepts +91XXXXXXXXXX, 91XXXXXXXXXX, 0XXXXXXXXXX or a bare
// 10-digit mobile starting 6–9.
func NormalizeIndia(raw string) (Destination, error) {
	s := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '-', '(', ')':
			return -1
		}
		return r
	}, strings.TrimSpace(raw))
	s = strings.TrimPrefix(s, "+")
	s = strings.TrimPrefix(s, "00")
	switch {
	case len(s) == 12 && strings.HasPrefix(s, "91"):
		s = s[2:]
	case len(s) == 11 && strings.HasPrefix(s, "0"):
		s = s[1:]
	}
	if len(s) != 10 || s[0] < '6' || s[0] > '9' || !digits(s) {
		return Destination{}, ErrInvalidDestination
	}
	return Destination{E164: "91" + s, MCC: "404", MNC: "*"}, nil
}

// ValidateSender accepts a 1–11 character alphanumeric header (DLT headers
// are 6 letters) or a 3–15 digit numeric sender.
func ValidateSender(s string) error {
	switch {
	case s == "":
		return ErrInvalidSender
	case digits(s):
		if len(s) < 3 || len(s) > 15 {
			return ErrInvalidSender
		}
	default:
		if len(s) > 11 {
			return ErrInvalidSender
		}
		for _, r := range s {
			if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
				return ErrInvalidSender
			}
		}
	}
	return nil
}

// IsNumeric reports whether s is all digits.
func IsNumeric(s string) bool { return s != "" && digits(s) }

func digits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
