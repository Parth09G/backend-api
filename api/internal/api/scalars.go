// Package api holds the request and response types of the REST contract
// (openapi.yaml components.schemas). Field names and JSON shapes must match
// the spec; the server tests validate responses against it.
package api

import (
	"time"

	"github.com/shopspring/decimal"
)

// Money is a rupee amount rendered as a 2-decimal string: "12500.50".
type Money decimal.Decimal

// Qty is a quantity rendered as a 3-decimal string: "1.500".
type Qty decimal.Decimal

// Rate is a rate or cost rendered as a 4-decimal string: "62.5000".
type Rate decimal.Decimal

func (m Money) MarshalJSON() ([]byte, error) {
	return []byte(`"` + decimal.Decimal(m).StringFixed(2) + `"`), nil
}
func (q Qty) MarshalJSON() ([]byte, error) {
	return []byte(`"` + decimal.Decimal(q).StringFixed(3) + `"`), nil
}
func (r Rate) MarshalJSON() ([]byte, error) {
	return []byte(`"` + decimal.Decimal(r).StringFixed(4) + `"`), nil
}

func (m Money) D() decimal.Decimal { return decimal.Decimal(m) }
func (q Qty) D() decimal.Decimal   { return decimal.Decimal(q) }
func (r Rate) D() decimal.Decimal  { return decimal.Decimal(r) }

func M(d decimal.Decimal) Money { return Money(d) }
func Q(d decimal.Decimal) Qty   { return Qty(d) }
func R(d decimal.Decimal) Rate  { return Rate(d) }

func MP(d *decimal.Decimal) *Money {
	if d == nil {
		return nil
	}
	m := Money(*d)
	return &m
}

func RP(d *decimal.Decimal) *Rate {
	if d == nil {
		return nil
	}
	r := Rate(*d)
	return &r
}

// Date is a calendar date rendered as "2026-10-10".
type Date time.Time

func (d Date) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Time(d).Format(time.DateOnly) + `"`), nil
}

func (d Date) T() time.Time { return time.Time(d) }

func DP(t *time.Time) *Date {
	if t == nil {
		return nil
	}
	d := Date(*t)
	return &d
}

// Ptr returns a pointer to v (for optional response fields).
func Ptr[T any](v T) *T { return &v }

// StrPtr returns nil for an empty string.
func StrPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
