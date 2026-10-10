// Package billing handles Cash and Account bills: calculate, create, draft,
// finalize, edit with revision history, cancel, copy to Account bill, PDF,
// e-invoice and e-way bill (FR-BL, FR-CB, FR-AB, FR-ED, FR-EI).
package billing

import (
	"fmt"
	"sort"

	"github.com/shopspring/decimal"
)

// The server is the only place tax and totals are calculated (API design 1).
// Rules (API design 6.4, FR-BL-04..06):
//   - line gross = qty x rate, rounded to paise;
//   - line discount = gross x discount_pct% + discount_amt;
//   - with GST-inclusive prices the net is divided by (1 + rate%);
//   - the bill discount is spread over the lines in proportion to their
//     taxable value (it reduces the taxable value, so GST is on the net);
//   - CGST and SGST are each half of GST (rounded per line); IGST when the
//     place of supply is another state;
//   - charges (labour, transport, other) are taxed at their own rate;
//   - the grand total is rounded to the nearest rupee (round_off).

var (
	hundred = decimal.NewFromInt(100)
	two     = decimal.NewFromInt(2)
)

type LineIn struct {
	Qty, Rate, DiscPct, DiscAmt, GSTRate decimal.Decimal
}

type ChargeIn struct {
	Amount, GSTRate decimal.Decimal
}

type CalcInput struct {
	Lines        []LineIn
	Charges      []ChargeIn
	BillDiscount decimal.Decimal
	Interstate   bool
	InclusiveGST bool
}

type LineOut struct {
	Gross, Discount, BillDiscountShare, Taxable, CGST, SGST, IGST, Total decimal.Decimal
}

type ChargeOut struct {
	Taxable, CGST, SGST, IGST decimal.Decimal
}

type TaxSplit struct {
	Rate, Taxable, CGST, SGST, IGST decimal.Decimal
}

type CalcResult struct {
	Lines          []LineOut
	Charges        []ChargeOut
	ItemsTaxable   decimal.Decimal
	ChargesTaxable decimal.Decimal
	BillDiscount   decimal.Decimal
	TaxableTotal   decimal.Decimal
	CGST, SGST     decimal.Decimal
	IGST           decimal.Decimal
	RoundOff       decimal.Decimal
	GrandTotal     decimal.Decimal
	TaxByRate      []TaxSplit
}

// CalcError points at the offending input.
type CalcError struct {
	Field   string
	Message string
}

func (e *CalcError) Error() string { return e.Field + ": " + e.Message }

func r2(d decimal.Decimal) decimal.Decimal { return d.Round(2) }

// tax splits GST on a taxable value.
func tax(taxable, rate decimal.Decimal, interstate bool) (cgst, sgst, igst decimal.Decimal) {
	if interstate {
		return decimal.Zero, decimal.Zero, r2(taxable.Mul(rate).Div(hundred))
	}
	half := r2(taxable.Mul(rate).Div(hundred).Div(two))
	return half, half, decimal.Zero
}

// Calculate computes line, charge and bill totals.
func Calculate(in CalcInput) (CalcResult, error) {
	res := CalcResult{Lines: make([]LineOut, len(in.Lines)), Charges: make([]ChargeOut, len(in.Charges))}
	pre := make([]decimal.Decimal, len(in.Lines))
	itemsPre := decimal.Zero
	for i, l := range in.Lines {
		gross := r2(l.Qty.Mul(l.Rate))
		disc := r2(gross.Mul(l.DiscPct).Div(hundred)).Add(l.DiscAmt)
		if disc.GreaterThan(gross) {
			return res, &CalcError{Field: fmt.Sprintf("lines[%d].discount_amt", i), Message: "discount is more than the line amount"}
		}
		net := gross.Sub(disc)
		if in.InclusiveGST {
			net = r2(net.Mul(hundred).Div(hundred.Add(l.GSTRate)))
		}
		pre[i] = net
		itemsPre = itemsPre.Add(net)
		res.Lines[i] = LineOut{Gross: gross, Discount: disc}
	}

	bd := r2(in.BillDiscount)
	if bd.GreaterThan(itemsPre) {
		return res, &CalcError{Field: "bill_discount", Message: "bill discount is more than the items' value"}
	}
	shares := spread(bd, pre)

	byRate := map[string]*TaxSplit{}
	add := func(rate, taxable, c, s, ig decimal.Decimal) {
		k := rate.String()
		t := byRate[k]
		if t == nil {
			t = &TaxSplit{Rate: rate}
			byRate[k] = t
		}
		t.Taxable = t.Taxable.Add(taxable)
		t.CGST, t.SGST, t.IGST = t.CGST.Add(c), t.SGST.Add(s), t.IGST.Add(ig)
		res.CGST, res.SGST, res.IGST = res.CGST.Add(c), res.SGST.Add(s), res.IGST.Add(ig)
	}

	for i, l := range in.Lines {
		taxable := pre[i].Sub(shares[i])
		c, s, ig := tax(taxable, l.GSTRate, in.Interstate)
		lo := &res.Lines[i]
		lo.BillDiscountShare, lo.Taxable, lo.CGST, lo.SGST, lo.IGST = shares[i], taxable, c, s, ig
		lo.Total = taxable.Add(c).Add(s).Add(ig)
		res.ItemsTaxable = res.ItemsTaxable.Add(taxable)
		add(l.GSTRate, taxable, c, s, ig)
	}
	for i, ch := range in.Charges {
		taxable := r2(ch.Amount)
		c, s, ig := tax(taxable, ch.GSTRate, in.Interstate)
		res.Charges[i] = ChargeOut{Taxable: taxable, CGST: c, SGST: s, IGST: ig}
		res.ChargesTaxable = res.ChargesTaxable.Add(taxable)
		add(ch.GSTRate, taxable, c, s, ig)
	}

	res.BillDiscount = bd
	res.TaxableTotal = res.ItemsTaxable.Add(res.ChargesTaxable)
	exact := res.TaxableTotal.Add(res.CGST).Add(res.SGST).Add(res.IGST)
	res.GrandTotal = exact.Round(0)
	res.RoundOff = res.GrandTotal.Sub(exact)

	for _, t := range byRate {
		res.TaxByRate = append(res.TaxByRate, *t)
	}
	sort.Slice(res.TaxByRate, func(i, j int) bool { return res.TaxByRate[i].Rate.LessThan(res.TaxByRate[j].Rate) })
	return res, nil
}

// spread divides amount over weights in proportion, rounding to paise; the
// rounding remainder goes to the largest weight. No share exceeds its weight.
func spread(amount decimal.Decimal, weights []decimal.Decimal) []decimal.Decimal {
	out := make([]decimal.Decimal, len(weights))
	total := decimal.Zero
	largest := -1
	for i, w := range weights {
		total = total.Add(w)
		if largest < 0 || w.GreaterThan(weights[largest]) {
			largest = i
		}
	}
	if amount.IsZero() || !total.IsPositive() {
		return out
	}
	given := decimal.Zero
	for i, w := range weights {
		if i == largest {
			continue
		}
		out[i] = decimal.Min(r2(amount.Mul(w).Div(total)), w)
		given = given.Add(out[i])
	}
	out[largest] = amount.Sub(given)
	return out
}
