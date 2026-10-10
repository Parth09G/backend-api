package num

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestWords(t *testing.T) {
	cases := map[string]string{
		"0":           "Rupees Zero Only",
		"10.50":       "Rupees Ten and Fifty Paise Only",
		"148450":      "Rupees One Lakh Forty Eight Thousand Four Hundred Fifty Only",
		"2832":        "Rupees Two Thousand Eight Hundred Thirty Two Only",
		"100000":      "Rupees One Lakh Only",
		"12345678.09": "Rupees One Crore Twenty Three Lakh Forty Five Thousand Six Hundred Seventy Eight and Nine Paise Only",
		"1000000000":  "Rupees One Hundred Crore Only",
		"19":          "Rupees Nineteen Only",
	}
	for in, want := range cases {
		if got := Words(decimal.RequireFromString(in)); got != want {
			t.Errorf("Words(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestTrimAndRound(t *testing.T) {
	if got := Trim(decimal.RequireFromString("18.00")); got != "18" {
		t.Errorf("Trim 18.00 = %s", got)
	}
	if got := Trim(decimal.RequireFromString("0.25")); got != "0.25" {
		t.Errorf("Trim 0.25 = %s", got)
	}
	if got := Money(decimal.RequireFromString("12500.5")).StringFixed(2); got != "12500.50" {
		t.Errorf("Money = %s", got)
	}
	if got := Money(decimal.RequireFromString("2.345")).StringFixed(2); got != "2.35" {
		t.Errorf("half-up = %s", got)
	}
	if got := Qty(decimal.RequireFromString("1.5")).StringFixed(3); got != "1.500" {
		t.Errorf("Qty = %s", got)
	}
}
