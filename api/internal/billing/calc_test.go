package billing

import (
	"testing"

	"github.com/shopspring/decimal"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func TestCalculate(t *testing.T) {
	cases := []struct {
		name string
		in   CalcInput
		want map[string]string // field -> expected (2 decimals)
	}{
		{
			name: "intra-state, one line, 18%",
			in:   CalcInput{Lines: []LineIn{{Qty: d("30"), Rate: d("80"), GSTRate: d("18")}}},
			want: map[string]string{"taxable": "2400.00", "cgst": "216.00", "sgst": "216.00", "igst": "0.00", "grand": "2832.00", "round": "0.00"},
		},
		{
			name: "inter-state uses IGST",
			in:   CalcInput{Interstate: true, Lines: []LineIn{{Qty: d("30"), Rate: d("80"), GSTRate: d("18")}}},
			want: map[string]string{"taxable": "2400.00", "cgst": "0.00", "igst": "432.00", "grand": "2832.00"},
		},
		{
			// API design 4.2 example: 1.5 t @ 68500, 50 bags @ 395 less 2%, labour 1200, transport 2500.
			name: "API design example with discount and charges",
			in: CalcInput{
				Lines: []LineIn{
					{Qty: d("1.500"), Rate: d("68500.00"), GSTRate: d("18")},
					{Qty: d("50"), Rate: d("395.00"), DiscPct: d("2"), GSTRate: d("18")},
				},
				Charges: []ChargeIn{{Amount: d("1200.00"), GSTRate: d("18")}, {Amount: d("2500.00"), GSTRate: d("18")}},
			},
			// items 102750 + 19355 = 122105; charges 3700; taxable 125805; GST 18% = 22644.90.
			want: map[string]string{"taxable": "125805.00", "cgst": "11322.45", "sgst": "11322.45", "grand": "148450.00", "round": "0.10"},
		},
		{
			name: "half-up rounding and round-off",
			in:   CalcInput{Lines: []LineIn{{Qty: d("3"), Rate: d("33.33"), GSTRate: d("5")}}},
			// gross 99.99; tax 5% = 4.9995 -> cgst/sgst 2.50 each; total 104.99 -> 105.00
			want: map[string]string{"taxable": "99.99", "cgst": "2.50", "sgst": "2.50", "grand": "105.00", "round": "0.01"},
		},
		{
			name: "bill discount spread over lines",
			in: CalcInput{BillDiscount: d("100"), Lines: []LineIn{
				{Qty: d("1"), Rate: d("300"), GSTRate: d("18")},
				{Qty: d("1"), Rate: d("700"), GSTRate: d("5")},
			}},
			// shares 30 / 70 -> taxable 270 @18 = 48.60, 630 @5 = 31.50
			want: map[string]string{"taxable": "900.00", "cgst": "40.05", "sgst": "40.05", "grand": "980.00"},
		},
		{
			name: "GST-inclusive rates",
			in:   CalcInput{InclusiveGST: true, Lines: []LineIn{{Qty: d("1"), Rate: d("118"), GSTRate: d("18")}}},
			want: map[string]string{"taxable": "100.00", "cgst": "9.00", "sgst": "9.00", "grand": "118.00"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := Calculate(c.in)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{
				"taxable": res.TaxableTotal.StringFixed(2), "cgst": res.CGST.StringFixed(2), "sgst": res.SGST.StringFixed(2),
				"igst": res.IGST.StringFixed(2), "grand": res.GrandTotal.StringFixed(2), "round": res.RoundOff.StringFixed(2),
			}
			for k, want := range c.want {
				if got[k] != want {
					t.Errorf("%s = %s, want %s", k, got[k], want)
				}
			}
		})
	}
}

func TestCalculateRejectsOverDiscount(t *testing.T) {
	_, err := Calculate(CalcInput{Lines: []LineIn{{Qty: d("1"), Rate: d("100"), DiscAmt: d("150"), GSTRate: d("18")}}})
	if ce, ok := err.(*CalcError); !ok || ce.Field != "lines[0].discount_amt" {
		t.Fatalf("want line discount error, got %v", err)
	}
	_, err = Calculate(CalcInput{BillDiscount: d("500"), Lines: []LineIn{{Qty: d("1"), Rate: d("100"), GSTRate: d("18")}}})
	if ce, ok := err.(*CalcError); !ok || ce.Field != "bill_discount" {
		t.Fatalf("want bill discount error, got %v", err)
	}
}

func TestSpread(t *testing.T) {
	got := spread(d("10"), []decimal.Decimal{d("1"), d("1"), d("1")})
	sum := got[0].Add(got[1]).Add(got[2])
	if !sum.Equal(d("10")) {
		t.Fatalf("shares %v sum to %s", got, sum)
	}
}
