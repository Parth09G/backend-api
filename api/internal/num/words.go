package num

import (
	"strings"

	"github.com/shopspring/decimal"
)

var ones = []string{"", "One", "Two", "Three", "Four", "Five", "Six", "Seven", "Eight", "Nine", "Ten",
	"Eleven", "Twelve", "Thirteen", "Fourteen", "Fifteen", "Sixteen", "Seventeen", "Eighteen", "Nineteen"}
var tens = []string{"", "", "Twenty", "Thirty", "Forty", "Fifty", "Sixty", "Seventy", "Eighty", "Ninety"}

// Words spells an amount in the Indian system (FR-BL-06):
// 148450 -> "Rupees One Lakh Forty Eight Thousand Four Hundred Fifty Only",
// 10.50  -> "Rupees Ten and Fifty Paise Only".
func Words(amount decimal.Decimal) string {
	amount = amount.Abs().Round(2)
	rupees := amount.Truncate(0)
	paise := amount.Sub(rupees).Mul(Hundred).IntPart()

	var b strings.Builder
	b.WriteString("Rupees ")
	if rupees.IsZero() {
		b.WriteString("Zero")
	} else {
		b.WriteString(indian(rupees.BigInt().Int64()))
	}
	if paise > 0 {
		b.WriteString(" and ")
		b.WriteString(below100(paise))
		b.WriteString(" Paise")
	}
	b.WriteString(" Only")
	return b.String()
}

// indian spells n with crore / lakh / thousand / hundred groups.
func indian(n int64) string {
	var parts []string
	if n >= 10000000 {
		parts = append(parts, indian(n/10000000)+" Crore")
		n %= 10000000
	}
	if n >= 100000 {
		parts = append(parts, below100(n/100000)+" Lakh")
		n %= 100000
	}
	if n >= 1000 {
		parts = append(parts, below100(n/1000)+" Thousand")
		n %= 1000
	}
	if n >= 100 {
		parts = append(parts, ones[n/100]+" Hundred")
		n %= 100
	}
	if n > 0 {
		parts = append(parts, below100(n))
	}
	return strings.Join(parts, " ")
}

func below100(n int64) string {
	if n < 20 {
		return ones[n]
	}
	if n%10 == 0 {
		return tens[n/10]
	}
	return tens[n/10] + " " + ones[n%10]
}
