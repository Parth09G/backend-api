// Package num holds decimal helpers for money, quantities and rates.
// Never use float64 for these (HLD 6.3); JSON carries them as strings.
package num

import (
	"strings"

	"github.com/shopspring/decimal"
)

var (
	Zero    = decimal.Zero
	Hundred = decimal.NewFromInt(100)
)

// Parse reads a decimal string that validation has already accepted.
// An empty string is zero.
func Parse(s string) decimal.Decimal {
	if s == "" {
		return decimal.Zero
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero
	}
	return d
}

// ParsePtr is Parse for optional fields.
func ParsePtr(s *string) *decimal.Decimal {
	if s == nil || *s == "" {
		return nil
	}
	d := Parse(*s)
	return &d
}

// Money rounds half away from zero to paise. JSON formatting with a fixed
// scale is done by the api.Money type, not here.
func Money(d decimal.Decimal) decimal.Decimal { return d.Round(2) }

// Qty rounds to 3 decimals ("1.500").
func Qty(d decimal.Decimal) decimal.Decimal { return d.Round(3) }

// Rate rounds to 4 decimals ("62.5000").
func Rate(d decimal.Decimal) decimal.Decimal { return d.Round(4) }

// Trim formats without trailing zeros: 18.00 -> "18", 0.25 -> "0.25".
// Used for GST rates and percentages.
func Trim(d decimal.Decimal) string {
	s := d.String()
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "-0" || s == "" {
		return "0"
	}
	return s
}

// TrimPtr is Trim for optional values.
func TrimPtr(d *decimal.Decimal) *string {
	if d == nil {
		return nil
	}
	s := Trim(*d)
	return &s
}

// MoneyPtr rounds an optional amount.
func MoneyPtr(d *decimal.Decimal) *decimal.Decimal {
	if d == nil {
		return nil
	}
	m := Money(*d)
	return &m
}

// RatePtr rounds an optional rate.
func RatePtr(d *decimal.Decimal) *decimal.Decimal {
	if d == nil {
		return nil
	}
	m := Rate(*d)
	return &m
}

// Sum adds decimals.
func Sum(ds ...decimal.Decimal) decimal.Decimal {
	s := decimal.Zero
	for _, d := range ds {
		s = s.Add(d)
	}
	return s
}

// Z dereferences an optional decimal, nil being zero (generated columns
// such as bill.balance_due are typed nullable).
func Z(d *decimal.Decimal) decimal.Decimal {
	if d == nil {
		return decimal.Zero
	}
	return *d
}
