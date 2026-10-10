package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/xuri/excelize/v2"
)

// TestAPI walks the business flows end to end through HTTP, in order.
// Each response is checked against openapi.yaml by the harness.
func TestAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: needs Docker")
	}
	e := startEnv(t)
	owner := e.owner()
	var admin *client

	num := func(v any) int64 {
		switch x := v.(type) {
		case float64:
			return int64(x)
		case json.Number:
			n, _ := x.Int64()
			return n
		}
		t.Fatalf("not a number: %#v", v)
		return 0
	}
	get := func(m map[string]any, path ...string) any {
		var cur any = m
		for _, p := range path {
			cur = cur.(map[string]any)[p]
		}
		return cur
	}
	stockAt := func(itemID int64, loc string) string {
		item := owner.get(t, fmt.Sprintf("/items/%d", itemID)).expect(200).m()
		for _, s := range item["stock"].([]any) {
			sm := s.(map[string]any)
			if sm["location_name"] == loc {
				return sm["qty"].(string)
			}
		}
		return "0.000"
	}
	balance := func(partyID int64) string {
		return owner.get(t, fmt.Sprintf("/parties/%d", partyID)).expect(200).m()["balance"].(string)
	}

	t.Run("auth and me", func(t *testing.T) {
		anon := &client{e: e}
		anon.get(t, "/me").expect(401).code("UNAUTHENTICATED")
		bad := &client{e: e, token: owner.token + "x"}
		bad.get(t, "/me").expect(401)
		res, _ := http.Get(e.api.URL + "/api/v1/health")
		if res.StatusCode != 200 {
			t.Fatalf("health %d", res.StatusCode)
		}
		me := owner.get(t, "/me").expect(200).m()
		if me["role"] != "super_admin" || !strings.Contains(fmt.Sprint(me["permissions"]), "bill.cancel") {
			t.Fatalf("me = %v", me)
		}
		if me["last_login_at"] == nil {
			t.Fatal("login not recorded")
		}
	})

	t.Run("settings", func(t *testing.T) {
		s := owner.put(t, "/settings", map[string]any{
			"shop_name": "Demo Building Materials", "address": "1 Market Road, Bengaluru 560001", "state_code": "29",
			"gstin": "29ABCDE1234F1Z5", "phone": "9800000000", "bank_name": "SBI", "upi_id": "shop@upi",
			"invoice_terms": "Goods once sold are not taken back.", "einvoice_enabled": true, "default_charge_gst": "18",
		}).expect(200).m()
		if s["gstin"] != "29ABCDE1234F1Z5" || s["default_charge_gst"] != "18" {
			t.Fatalf("settings %v", s)
		}
		owner.put(t, "/settings", map[string]any{"shop_name": "X", "address": "Y", "state_code": "29", "gstin": "bad"}).
			expect(400).code("VALIDATION_FAILED")
		// Logo: a 1x1 PNG.
		png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")
		logo := owner.upload(t, http.MethodPut, "/settings/logo", nil, "file", "logo.png", png).expect(200).m()
		if logo["logo_url"] == nil {
			t.Fatal("logo url missing")
		}
		owner.put(t, "/settings/einvoice-credentials", map[string]any{"provider": "mock", "environment": "sandbox",
			"username": "gsp-user", "password": "secret"}).expect(204)
		test := owner.post(t, "/settings/einvoice-credentials/test", nil).expect(200).m()
		if test["ok"] != true {
			t.Fatalf("credential test %v", test)
		}
		owner.put(t, "/settings/tally-mapping", map[string]any{"sales_ledger_cash": "Sales A/c", "cash_ledger": "Cash"}).expect(200)
		if m := owner.get(t, "/settings/tally-mapping").expect(200).m(); m["sales_ledger_cash"] != "Sales A/c" {
			t.Fatalf("mapping %v", m)
		}
	})

	t.Run("users", func(t *testing.T) {
		u := owner.post(t, "/users", map[string]any{"full_name": "Asha Admin", "email": "admin@shop.in", "role": "admin"}).expect(201).m()
		admin = e.client(uuid.MustParse(u["id"].(string)))
		owner.post(t, "/users", map[string]any{"full_name": "Dup", "email": "admin@shop.in", "role": "admin"}).expect(409).code("DUPLICATE")
		owner.patch(t, "/users/"+u["id"].(string), map[string]any{"mobile": "9811111111"}).expect(200)
		admin.get(t, "/users").expect(403).code("FORBIDDEN")
		if me := admin.get(t, "/me").expect(200).m(); strings.Contains(fmt.Sprint(me["permissions"]), "bill.cancel") {
			t.Fatal("admin must not have bill.cancel")
		}
		owner.post(t, "/users/"+owner.id.String()+"/deactivate", nil).expect(409)
		owner.post(t, "/users/"+u["id"].(string)+"/deactivate", nil).expect(204)
		admin.get(t, "/me").expect(401)
		owner.post(t, "/users/"+u["id"].(string)+"/reactivate", nil).expect(204)
		admin.get(t, "/me").expect(200)
		owner.post(t, "/users/"+u["id"].(string)+"/password-reset", nil).expect(204)
		if len(owner.get(t, "/users").expect(200).body) == 0 {
			t.Fatal("users list empty")
		}
	})

	var godown1, shop, yard int64
	t.Run("locations", func(t *testing.T) {
		var locs []map[string]any
		owner.get(t, "/locations").expect(200).into(&locs)
		for _, l := range locs {
			switch l["name"] {
			case "Shop":
				shop = num(l["id"])
			case "Godown 1":
				godown1 = num(l["id"])
			}
		}
		y := owner.post(t, "/locations", map[string]any{"name": "Yard", "kind": "godown"}).expect(201).m()
		yard = num(y["id"])
		owner.patch(t, fmt.Sprintf("/locations/%d", yard), map[string]any{"name": "Yard", "kind": "godown", "address": "Hoskote"}).expect(200)
		admin.post(t, "/locations", map[string]any{"name": "No", "kind": "godown"}).expect(403)
	})

	units := map[string]int64{}
	var cementCat, steelCat int64
	t.Run("units and categories", func(t *testing.T) {
		var list []map[string]any
		owner.get(t, "/units").expect(200).into(&list)
		for _, u := range list {
			units[u["symbol"].(string)] = num(u["id"])
		}
		owner.post(t, "/units", map[string]any{"name": "Bag", "symbol": "bag2"}).expect(409).code("DUPLICATE")
		c := admin.post(t, "/categories", map[string]any{
			"name": "Cement", "default_hsn": "2523", "default_gst": "18",
			"units": []any{map[string]any{"unit_id": units["bag"], "is_default": true}},
			"attributes": []any{map[string]any{"code": "brand", "label": "Brand", "data_type": "text", "is_required": true},
				map[string]any{"code": "grade", "label": "Grade", "data_type": "select", "options": []string{"OPC 53", "PPC"}, "is_required": true}},
		}).expect(201).m()
		cementCat = num(c["id"])
		s := admin.post(t, "/categories", map[string]any{
			"name": "Steel", "default_hsn": "7214", "default_gst": "18",
			"units": []any{map[string]any{"unit_id": units["kg"], "is_default": true}, map[string]any{"unit_id": units["ton"]},
				map[string]any{"new_unit": map[string]any{"name": "Piece", "symbol": "pc", "uqc_code": "pcs", "decimals": 0}}},
			"attributes": []any{map[string]any{"code": "brand", "label": "Brand", "data_type": "text", "is_required": true},
				map[string]any{"code": "size", "label": "Size", "data_type": "number", "value_unit": "mm", "is_required": true}},
		}).expect(201).m()
		steelCat = num(s["id"])
		if len(s["units"].([]any)) != 3 {
			t.Fatalf("steel units %v", s["units"])
		}
		attr := admin.post(t, fmt.Sprintf("/categories/%d/attributes", steelCat), map[string]any{"code": "finish", "label": "Finish", "data_type": "text"}).expect(201).m()
		admin.patch(t, fmt.Sprintf("/categories/%d/attributes/%d", steelCat, num(attr["id"])),
			map[string]any{"code": "finish", "label": "Surface finish", "data_type": "text"}).expect(200)
		admin.do(t, http.MethodDelete, fmt.Sprintf("/categories/%d/attributes/%d", steelCat, num(attr["id"])), nil, nil).expect(403)
		owner.do(t, http.MethodDelete, fmt.Sprintf("/categories/%d/attributes/%d", steelCat, num(attr["id"])), nil, nil).expect(204)
		tmpl := admin.get(t, fmt.Sprintf("/categories/%d/import-template", cementCat)).expect(200)
		if !bytes.HasPrefix(tmpl.body, []byte("PK")) {
			t.Fatal("template is not xlsx")
		}
		var cats []map[string]any
		admin.get(t, "/categories").expect(200).into(&cats)
		if len(cats) != 2 {
			t.Fatalf("categories %d", len(cats))
		}
	})

	var cement, steel int64
	t.Run("items", func(t *testing.T) {
		c := admin.post(t, "/items", map[string]any{"category_id": cementCat, "attributes": map[string]any{"brand": "UltraTech", "grade": "opc 53"},
			"hsn_code": "2523", "gst_rate": "18", "base_unit_id": units["bag"], "reorder_level": "20",
			"opening_stock": []any{map[string]any{"location_id": godown1, "qty": "100", "unit_cost": "350"}}}).expect(201).m()
		cement = num(c["id"])
		if c["name"] != "UltraTech OPC 53" || c["stock_total"] != "100.000" || c["avg_cost"] != "350.0000" {
			t.Fatalf("cement %v", c)
		}
		admin.post(t, "/items", map[string]any{"category_id": cementCat, "attributes": map[string]any{"brand": "UltraTech", "grade": "OPC 53"},
			"hsn_code": "2523", "gst_rate": "18", "base_unit_id": units["bag"]}).expect(409).code("DUPLICATE")
		admin.post(t, "/items", map[string]any{"category_id": cementCat, "attributes": map[string]any{"brand": "ACC", "grade": "OPC 99"},
			"hsn_code": "2523", "gst_rate": "18", "base_unit_id": units["bag"]}).expect(400).code("VALIDATION_FAILED")
		s := admin.post(t, "/items", map[string]any{"category_id": steelCat, "attributes": map[string]any{"brand": "JSW", "size": 12},
			"hsn_code": "7214", "gst_rate": "18", "base_unit_id": units["kg"], "reorder_level": "500",
			"units":         []any{map[string]any{"unit_id": units["ton"], "factor_to_base": "1000"}},
			"opening_stock": []any{map[string]any{"location_id": godown1, "qty": "2000", "unit_cost": "60"}}}).expect(201).m()
		steel = num(s["id"])
		if s["name"] != "JSW 12 mm" {
			t.Fatalf("steel name %v", s["name"])
		}
		list := admin.get(t, "/items?q=jsw").expect(200).m()
		if num(get(list, "meta", "total")) != 1 {
			t.Fatalf("search %v", list["meta"])
		}
		if num(get(admin.get(t, "/items?attr[brand]=ultratech").expect(200).m(), "meta", "total")) != 1 {
			t.Fatal("attr filter")
		}
		admin.get(t, "/items?stock_status=low&sort=-stock_value").expect(200)
		// Factors are frozen after movements.
		admin.patch(t, fmt.Sprintf("/items/%d", steel), map[string]any{"category_id": steelCat, "attributes": map[string]any{"brand": "JSW", "size": 12},
			"hsn_code": "7214", "gst_rate": "18", "base_unit_id": units["kg"],
			"units": []any{map[string]any{"unit_id": units["ton"], "factor_to_base": "999"}}}).expect(409)
		admin.patch(t, fmt.Sprintf("/items/%d", steel), map[string]any{"category_id": steelCat, "attributes": map[string]any{"brand": "JSW", "size": 12},
			"hsn_code": "7214", "gst_rate": "18", "base_unit_id": units["kg"], "reorder_level": "400",
			"units": []any{map[string]any{"unit_id": units["ton"], "factor_to_base": "1000"}}}).expect(200)
		mv := admin.get(t, fmt.Sprintf("/items/%d/movements", cement)).expect(200).m()
		if num(get(mv, "meta", "total")) != 1 {
			t.Fatalf("movements %v", mv)
		}
		admin.get(t, fmt.Sprintf("/items/%d/price-hints?unit_id=%d", steel, units["ton"])).expect(200)
	})

	var ravi, kumar, dealer int64
	t.Run("parties", func(t *testing.T) {
		ravi = num(admin.post(t, "/parties", map[string]any{"kind": "customer", "name": "Ravi Builders", "gstin": "29AAAPR1234C1Z5",
			"billing_address": "Yelahanka, Bengaluru 560064", "mobile": "9845012345", "credit_limit": "500000",
			"sites": []any{map[string]any{"label": "Plot 21", "address": "Plot 21, Yelahanka"}}}).expect(201).m()["id"])
		kumar = num(admin.post(t, "/parties", map[string]any{"kind": "customer", "name": "Kumar", "credit_limit": "1000", "state_code": "29"}).expect(201).m()["id"])
		dealer = num(admin.post(t, "/parties", map[string]any{"kind": "supplier", "name": "Southern Cement", "gstin": "29AABCS5678D1Z2"}).expect(201).m()["id"])
		admin.post(t, "/parties", map[string]any{"kind": "customer", "name": "Mismatch", "gstin": "29AAAPR1234C1Z6", "state_code": "27"}).expect(400)
		admin.post(t, "/parties", map[string]any{"kind": "customer", "name": "Dup GSTIN", "gstin": "29AAAPR1234C1Z5"}).expect(409).code("DUPLICATE")
		p := admin.get(t, fmt.Sprintf("/parties/%d", ravi)).expect(200).m()
		if len(p["sites"].([]any)) != 1 || p["state_code"] != "29" {
			t.Fatalf("party %v", p)
		}
		admin.get(t, "/parties?kind=customer&q=ravi").expect(200)
	})

	var purchaseID int64
	t.Run("purchase updates stock and average cost", func(t *testing.T) {
		p := admin.post(t, "/purchases", map[string]any{"supplier_id": dealer, "supplier_invoice_no": "SC/1",
			"supplier_invoice_dt": "2026-10-09", "location_id": godown1, "freight": "1000",
			"lines": []any{map[string]any{"item_id": cement, "qty": "100", "unit_id": units["bag"], "rate": "300"}}}).expect(201).m()
		purchaseID = num(p["id"])
		if l := p["lines"].([]any)[0].(map[string]any); l["landed_unit_cost"] != "310.0000" {
			t.Fatalf("landed %v", l)
		}
		if g := get(p, "totals", "grand_total"); g != "36400.00" { // 30000 + 18% + 1000 freight
			t.Fatalf("purchase total %v", g)
		}
		item := admin.get(t, fmt.Sprintf("/items/%d", cement)).expect(200).m()
		if item["avg_cost"] != "330.0000" || item["stock_total"] != "200.000" {
			t.Fatalf("after purchase %v %v", item["avg_cost"], item["stock_total"])
		}
		admin.post(t, "/purchases", map[string]any{"supplier_id": dealer, "supplier_invoice_no": "SC/1",
			"supplier_invoice_dt": "2026-10-09", "location_id": godown1,
			"lines": []any{map[string]any{"item_id": cement, "qty": "1", "unit_id": units["bag"], "rate": "300"}}}).expect(409).code("DUPLICATE")
		if b := balance(dealer); b != "-36400.00" {
			t.Fatalf("dealer balance %s", b)
		}
	})

	cashBill := func(loc int64, customer map[string]any, extra map[string]any) map[string]any {
		b := map[string]any{"bill_type": "cash", "status": "active", "customer": customer, "place_of_supply": "29",
			"lines": []any{map[string]any{"item_id": cement, "location_id": loc, "qty": "10", "unit_id": units["bag"], "rate": "400"}}}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}

	var bill1 map[string]any
	t.Run("cash bill: shortage, save, e-invoice", func(t *testing.T) {
		calc := admin.post(t, "/bills/calculate", cashBill(shop, map[string]any{"party_id": ravi}, nil)).expect(200).m()
		if len(calc["stock_warnings"].([]any)) != 1 {
			t.Fatalf("calculate warnings %v", calc["stock_warnings"])
		}
		short := admin.post(t, "/bills", cashBill(shop, map[string]any{"party_id": ravi}, nil)).expect(422).code("INSUFFICIENT_STOCK").m()
		sl := short["short_lines"].([]any)[0].(map[string]any)
		if short["alert_sent"] != true || sl["available"] != "0.000" || len(sl["available_elsewhere"].([]any)) != 1 {
			t.Fatalf("short %v", short)
		}
		n := owner.get(t, "/notifications?unread_only=true").expect(200).m()
		if num(get(n, "meta", "total")) < 1 {
			t.Fatal("no shortage notification")
		}
		body := cashBill(godown1, map[string]any{"party_id": ravi, "site_address": "Plot 21"}, map[string]any{
			"vehicle_no": "KA05AB1234",
			"lines": []any{
				map[string]any{"item_id": cement, "location_id": godown1, "qty": "10", "unit_id": units["bag"], "rate": "400"},
				map[string]any{"item_id": steel, "location_id": godown1, "qty": "1.500", "unit_id": units["ton"], "rate": "68500", "discount_pct": "2"},
			},
			"charges":  []any{map[string]any{"kind": "labour", "amount": "1200"}},
			"payments": []any{map[string]any{"mode": "upi", "amount": "50000", "reference": "UPI/1"}},
		})
		key := uuid.NewString()
		res := admin.do(t, http.MethodPost, "/bills", body, map[string]string{"Idempotency-Key": key}).expect(201)
		bill1 = res.m()
		if res.header.Get("ETag") != `"1"` || !strings.HasPrefix(bill1["bill_no"].(string), "C/") {
			t.Fatalf("bill %v etag %q", bill1["bill_no"], res.header.Get("ETag"))
		}
		// 4000 + 100695 (102750 less 2%) + 1200 = 105895; 18% GST = 19061.10; total 124956.10 -> 124956
		if g := get(bill1, "totals", "grand_total"); g != "124956.00" {
			t.Fatalf("grand total %v", g)
		}
		if b := get(bill1, "totals", "balance_due"); b != "74956.00" {
			t.Fatalf("balance due %v", b)
		}
		if get(bill1, "einvoice", "status") != "pending" {
			t.Fatalf("einvoice %v", bill1["einvoice"])
		}
		// Replay with the same key returns the same bill.
		rep := admin.do(t, http.MethodPost, "/bills", body, map[string]string{"Idempotency-Key": key}).expect(201)
		if rep.header.Get("Idempotent-Replay") != "true" || num(rep.m()["id"]) != num(bill1["id"]) {
			t.Fatal("idempotent replay failed")
		}
		body["notes"] = "changed"
		admin.do(t, http.MethodPost, "/bills", body, map[string]string{"Idempotency-Key": key}).expect(409).code("IDEMPOTENCY_REPLAY_MISMATCH")

		if s := stockAt(cement, "Godown 1"); s != "190.000" {
			t.Fatalf("cement at godown %s", s)
		}
		if s := stockAt(steel, "Godown 1"); s != "500.000" {
			t.Fatalf("steel at godown %s", s)
		}
		if b := balance(ravi); b != "74956.00" {
			t.Fatalf("ravi balance %s", b)
		}
		id := num(bill1["id"])
		eventually(t, "IRN", func() bool {
			b := admin.get(t, fmt.Sprintf("/bills/%d", id)).expect(200).m()
			bill1 = b
			return get(b, "einvoice", "status") == "generated"
		})
		if get(bill1, "einvoice", "signed_qr") == nil || get(bill1, "einvoice", "cancel_deadline") == nil {
			t.Fatalf("einvoice %v", bill1["einvoice"])
		}
		link := admin.get(t, fmt.Sprintf("/bills/%d/pdf?copy=duplicate", id)).expect(200).m()
		if !bytes.HasPrefix(e.sb.file(t, link["url"].(string)), []byte("%PDF")) {
			t.Fatal("invoice is not a PDF")
		}
		share := admin.post(t, fmt.Sprintf("/bills/%d/share", id), nil).expect(200).m()
		if !strings.HasPrefix(share["whatsapp_url"].(string), "https://wa.me/919845012345?text=") {
			t.Fatalf("share %v", share)
		}
	})

	t.Run("walk-in and credit limit", func(t *testing.T) {
		admin.post(t, "/bills", cashBill(godown1, map[string]any{"walk_in_name": "Walk-in"}, map[string]any{
			"payments": []any{map[string]any{"mode": "cash", "amount": "100"}}})).expect(400)
		w := admin.post(t, "/bills", cashBill(godown1, map[string]any{"walk_in_name": "Walk-in"}, map[string]any{
			"payments": []any{map[string]any{"mode": "cash", "amount": "4720"}}, "expected_total": "4720.00"})).expect(201).m()
		if get(w, "einvoice", "status") != "not_required" || get(w, "totals", "balance_due") != "0.00" {
			t.Fatalf("walk-in %v", w["totals"])
		}
		admin.post(t, "/bills", cashBill(godown1, map[string]any{"party_id": kumar}, nil)).expect(422).code("CREDIT_LIMIT_EXCEEDED")
		admin.post(t, "/bills", cashBill(godown1, map[string]any{"party_id": kumar}, map[string]any{"override_credit_limit": true})).expect(422)
		owner.post(t, "/bills", cashBill(godown1, map[string]any{"party_id": kumar}, map[string]any{"override_credit_limit": true})).expect(201)
		admin.post(t, "/bills", cashBill(godown1, map[string]any{"walk_in_name": "W"}, map[string]any{"expected_total": "1.00",
			"payments": []any{map[string]any{"mode": "cash", "amount": "4720"}}})).expect(400).code("VALIDATION_FAILED")
	})

	var acctBill map[string]any
	t.Run("draft, update and finalize an Account bill", func(t *testing.T) {
		d := admin.post(t, "/bills", map[string]any{"bill_type": "account", "status": "draft", "customer": map[string]any{"party_id": ravi},
			"place_of_supply": "29", "lines": []any{map[string]any{"item_id": cement, "qty": "5", "unit_id": units["bag"], "rate": "410"}}}).expect(201).m()
		if d["bill_no"] != nil || d["status"] != "draft" {
			t.Fatalf("draft %v", d)
		}
		id := num(d["id"])
		admin.do(t, http.MethodPut, fmt.Sprintf("/bills/%d", id), map[string]any{"bill_type": "account", "status": "draft",
			"customer": map[string]any{"party_id": ravi}, "place_of_supply": "29",
			"lines": []any{map[string]any{"item_id": cement, "qty": "6", "unit_id": units["bag"], "rate": "410"}}}, map[string]string{"If-Match": `"1"`}).expect(200)
		before := stockAt(cement, "Shop")
		acctBill = admin.post(t, fmt.Sprintf("/bills/%d/finalize", id), nil).expect(200).m()
		if !strings.HasPrefix(acctBill["bill_no"].(string), "A/") || acctBill["affects_stock"] != false {
			t.Fatalf("account bill %v", acctBill)
		}
		if stockAt(cement, "Shop") != before {
			t.Fatal("account bill changed stock")
		}
		eventually(t, "account IRN", func() bool {
			acctBill = admin.get(t, fmt.Sprintf("/bills/%d", id)).expect(200).m()
			return get(acctBill, "einvoice", "status") == "generated"
		})
		admin.post(t, fmt.Sprintf("/bills/%d/ewaybill", id), map[string]any{"vehicle_no": "ka05ab1234", "distance_km": 30}).expect(202)
		eventually(t, "e-way bill", func() bool {
			b := admin.get(t, fmt.Sprintf("/bills/%d", id)).expect(200).m()
			return get(b, "einvoice", "ewb_no") != nil
		})
		admin.post(t, fmt.Sprintf("/bills/%d/einvoice/cancel", id), map[string]any{"reason": "wrong rate"}).expect(403)
		owner.post(t, fmt.Sprintf("/bills/%d/einvoice/cancel", id), map[string]any{"reason": "wrong rate"}).expect(202)
		eventually(t, "IRN cancelled", func() bool {
			b := admin.get(t, fmt.Sprintf("/bills/%d", id)).expect(200).m()
			return get(b, "einvoice", "status") == "cancelled"
		})
	})

	var copyID int64
	t.Run("copy to Account bill", func(t *testing.T) {
		id := num(bill1["id"])
		bal := balance(ravi)
		stock := stockAt(cement, "Godown 1")
		c := admin.post(t, fmt.Sprintf("/bills/%d/copy-to-account", id), map[string]any{"status": "active"}).expect(201).m()
		copyID = num(c["id"])
		if c["posts_to_ledger"] != false || num(get(c, "source_bill", "id")) != id || get(c, "einvoice", "status") != "not_required" {
			t.Fatalf("copy %v", c)
		}
		if balance(ravi) != bal || stockAt(cement, "Godown 1") != stock {
			t.Fatal("copy changed ledger or stock")
		}
		admin.post(t, fmt.Sprintf("/bills/%d/copy-to-account", id), nil).expect(409).code("INVALID_STATE")
		src := admin.get(t, fmt.Sprintf("/bills/%d", id)).expect(200).m()
		if num(get(src, "account_copy", "id")) != copyID {
			t.Fatal("source does not link to copy")
		}
	})

	t.Run("edit an active bill", func(t *testing.T) {
		id := num(bill1["id"])
		edit := map[string]any{"bill_type": "cash", "status": "active", "customer": map[string]any{"party_id": ravi},
			"place_of_supply": "29", "reason": "Rate corrected after customer call",
			"lines":    []any{map[string]any{"item_id": cement, "location_id": godown1, "qty": "12", "unit_id": units["bag"], "rate": "390"}},
			"payments": []any{map[string]any{"mode": "upi", "amount": "1000"}}}
		admin.do(t, http.MethodPut, fmt.Sprintf("/bills/%d", id), edit, map[string]string{"If-Match": `"1"`}).expect(409).code("EDIT_NOT_ALLOWED")
		owner.do(t, http.MethodPut, fmt.Sprintf("/bills/%d", id), edit, map[string]string{"If-Match": `"7"`}).expect(412).code("VERSION_MISMATCH")
		res := owner.do(t, http.MethodPut, fmt.Sprintf("/bills/%d", id), edit, map[string]string{"If-Match": `"1"`}).expect(200)
		b := res.m()
		if b["version"] != float64(2) || res.header.Get("ETag") != `"2"` {
			t.Fatalf("edit version %v", b["version"])
		}
		// 170 before (two more bills since): old 10 bags and the steel return, 12 bags go out.
		if s := stockAt(cement, "Godown 1"); s != "168.000" {
			t.Fatalf("cement after edit %s", s)
		}
		if s := stockAt(steel, "Godown 1"); s != "2000.000" {
			t.Fatalf("steel after edit %s", s)
		}
		var revs []map[string]any
		owner.get(t, fmt.Sprintf("/bills/%d/revisions", id)).expect(200).into(&revs)
		if len(revs) != 1 || revs[0]["reason"] != "Rate corrected after customer call" {
			t.Fatalf("revisions %v", revs)
		}
		eventually(t, "IRN regenerated", func() bool {
			bill1 = owner.get(t, fmt.Sprintf("/bills/%d", id)).expect(200).m()
			return get(bill1, "einvoice", "status") == "generated"
		})
		// 24-hour window closed: edit and cancel are refused.
		e.clock.Advance(25 * time.Hour)
		owner.do(t, http.MethodPut, fmt.Sprintf("/bills/%d", id), edit, map[string]string{"If-Match": `"2"`}).expect(409).code("EINVOICE_WINDOW_CLOSED")
		if strings.Contains(fmt.Sprint(owner.get(t, fmt.Sprintf("/bills/%d", id)).expect(200).m()["allowed_actions"]), "edit") {
			t.Fatal("edit still allowed after the window")
		}
		e.clock.Reset()
	})

	t.Run("cancel", func(t *testing.T) {
		id := num(bill1["id"])
		owner.do(t, http.MethodPost, fmt.Sprintf("/bills/%d/cancel", id), map[string]any{"reason": "customer cancelled"},
			map[string]string{"If-Match": `"2"`}).expect(409).code("INVALID_STATE") // active copy
		admin.do(t, http.MethodPost, fmt.Sprintf("/bills/%d/cancel", copyID), map[string]any{"reason": "dup"}, map[string]string{"If-Match": `"1"`}).expect(403)
		owner.do(t, http.MethodPost, fmt.Sprintf("/bills/%d/cancel", copyID), map[string]any{"reason": "not needed"},
			map[string]string{"If-Match": `"1"`}).expect(200)
		c := owner.do(t, http.MethodPost, fmt.Sprintf("/bills/%d/cancel", id), map[string]any{"reason": "customer cancelled"},
			map[string]string{"If-Match": `"2"`}).expect(200).m()
		if c["status"] != "cancelled" || c["cancel_reason"] != "customer cancelled" {
			t.Fatalf("cancel %v", c)
		}
		if s := stockAt(cement, "Godown 1"); s != "180.000" {
			t.Fatalf("cement after cancel %s", s)
		}
		eventually(t, "IRN cancel", func() bool {
			b := owner.get(t, fmt.Sprintf("/bills/%d", id)).expect(200).m()
			return get(b, "einvoice", "status") == "cancelled"
		})
		link := owner.get(t, fmt.Sprintf("/bills/%d/pdf", id)).expect(200).m()
		if !bytes.HasPrefix(e.sb.file(t, link["url"].(string)), []byte("%PDF")) {
			t.Fatal("cancelled invoice PDF")
		}
		// Drafts are deleted, active bills are not.
		d := admin.post(t, "/bills", map[string]any{"bill_type": "cash", "status": "draft", "customer": map[string]any{"walk_in_name": "X"},
			"place_of_supply": "29", "lines": []any{map[string]any{"item_id": cement, "qty": "1", "unit_id": units["bag"], "rate": "1"}}}).expect(201).m()
		admin.do(t, http.MethodDelete, fmt.Sprintf("/bills/%d", num(d["id"])), nil, nil).expect(204)
		admin.do(t, http.MethodDelete, fmt.Sprintf("/bills/%d", num(acctBill["id"])), nil, nil).expect(409)
		list := admin.get(t, "/bills?bill_type=cash&status=cancelled&sort=-bill_date").expect(200).m()
		if num(get(list, "meta", "total")) != 1 {
			t.Fatalf("cancelled list %v", list["meta"])
		}
	})

	var ravBill map[string]any
	t.Run("payments", func(t *testing.T) {
		ravBill = admin.post(t, "/bills", cashBill(godown1, map[string]any{"party_id": ravi}, nil)).expect(201).m()
		before := balance(ravi)
		p := admin.post(t, "/payments", map[string]any{"direction": "received", "party_id": ravi, "mode": "upi", "amount": "1000",
			"reference": "UTR1", "auto_allocate": true}).expect(201).m()
		if len(p["allocations"].([]any)) == 0 || p["unallocated"] != "0.00" {
			t.Fatalf("auto allocate %v", p)
		}
		chq := admin.post(t, "/payments", map[string]any{"direction": "received", "party_id": ravi, "mode": "cheque", "amount": "500",
			"cheque_no": "000123", "bank_name": "HDFC", "allocations": []any{map[string]any{"ref_type": "cash_bill", "ref_id": num(ravBill["id"]), "amount": "500"}}}).expect(201).m()
		admin.post(t, fmt.Sprintf("/payments/%d/cheque-status", num(chq["id"])), map[string]any{"status": "bounced"}).expect(200)
		admin.post(t, fmt.Sprintf("/payments/%d/cheque-status", num(chq["id"])), map[string]any{"status": "cleared"}).expect(409)
		admin.post(t, fmt.Sprintf("/payments/%d/cancel", num(p["id"])), map[string]any{"reason": "entered twice"}).expect(403)
		owner.post(t, fmt.Sprintf("/payments/%d/cancel", num(p["id"])), map[string]any{"reason": "entered twice"}).expect(200)
		if balance(ravi) != before {
			t.Fatalf("balance after bounce+cancel %s, want %s", balance(ravi), before)
		}
		admin.post(t, "/payments", map[string]any{"direction": "paid", "party_id": dealer, "mode": "bank_transfer", "amount": "10000",
			"allocations": []any{map[string]any{"ref_type": "purchase", "ref_id": purchaseID, "amount": "10000"}}}).expect(201)
		admin.post(t, "/payments", map[string]any{"direction": "received", "party_id": ravi, "mode": "cash", "amount": "10",
			"allocations": []any{map[string]any{"ref_type": "account_bill", "ref_id": num(acctBill["id"]), "amount": "10"}}}).expect(400)
		pu := admin.get(t, fmt.Sprintf("/purchases/%d", purchaseID)).expect(200).m()
		if pu["balance_payable"] != "26400.00" {
			t.Fatalf("payable %v", pu["balance_payable"])
		}
		admin.get(t, "/payments?direction=received&cheque_status=bounced").expect(200)
		link := admin.get(t, fmt.Sprintf("/payments/%d/receipt", num(chq["id"]))).expect(200).m()
		if !bytes.HasPrefix(e.sb.file(t, link["url"].(string)), []byte("%PDF")) {
			t.Fatal("receipt PDF")
		}
		l := admin.get(t, fmt.Sprintf("/parties/%d/ledger", ravi)).expect(200).m()
		if l["closing"] != balance(ravi) {
			t.Fatalf("ledger closing %v vs balance %s", l["closing"], balance(ravi))
		}
		admin.get(t, fmt.Sprintf("/parties/%d/statement", ravi)).expect(200)
	})

	t.Run("returns and credit notes", func(t *testing.T) {
		billID := num(ravBill["id"])
		lineID := num(ravBill["lines"].([]any)[0].(map[string]any)["id"])
		stock := stockAt(cement, "Godown 1")
		bal := balance(ravi)
		r := admin.post(t, "/returns", map[string]any{"bill_id": billID, "reason": "two bags damaged in transit",
			"settlement": "adjust_ledger", "lines": []any{map[string]any{"bill_line_id": lineID, "qty": "2", "unit_id": units["bag"]}}}).expect(201).m()
		if r["status"] != "pending" || r["cn_no"] != nil {
			t.Fatalf("return %v", r)
		}
		if stockAt(cement, "Godown 1") != stock {
			t.Fatal("pending return changed stock")
		}
		admin.post(t, fmt.Sprintf("/returns/%d/approve", num(r["id"])), nil).expect(403)
		a := owner.post(t, fmt.Sprintf("/returns/%d/approve", num(r["id"])), nil).expect(200).m()
		if a["status"] != "approved" || !strings.HasPrefix(fmt.Sprint(a["cn_no"]), "CN/") {
			t.Fatalf("approved %v", a)
		}
		owner.post(t, fmt.Sprintf("/returns/%d/approve", num(r["id"])), nil).expect(409)
		// 2 bags x 400 = 800 + 18% = 944
		if get(a, "totals", "grand_total") != "944.00" {
			t.Fatalf("credit note total %v", a["totals"])
		}
		inc := func(s string, n int) string {
			var v float64
			fmt.Sscan(s, &v)
			return fmt.Sprintf("%.3f", v+float64(n))
		}
		if stockAt(cement, "Godown 1") != inc(stock, 2) {
			t.Fatalf("stock after return %s", stockAt(cement, "Godown 1"))
		}
		if balance(ravi) == bal {
			t.Fatal("ledger not credited")
		}
		eventually(t, "credit note IRN", func() bool {
			cn := admin.get(t, fmt.Sprintf("/returns/%d", num(r["id"]))).expect(200).m()
			return get(cn, "einvoice", "status") == "generated"
		})
		admin.post(t, "/returns", map[string]any{"bill_id": billID, "reason": "too many", "settlement": "adjust_ledger",
			"lines": []any{map[string]any{"bill_line_id": lineID, "qty": "9", "unit_id": units["bag"]}}}).expect(400)
		r2 := admin.post(t, "/returns", map[string]any{"bill_id": billID, "reason": "wrong item", "settlement": "refund",
			"refund": map[string]any{"mode": "cash", "amount": "100"},
			"lines":  []any{map[string]any{"bill_line_id": lineID, "qty": "1", "unit_id": units["bag"], "restock": false}}}).expect(201).m()
		owner.post(t, fmt.Sprintf("/returns/%d/reject", num(r2["id"])), map[string]any{"reason": "not our goods"}).expect(200)
		admin.get(t, "/returns?status=approved").expect(200)
		link := admin.get(t, fmt.Sprintf("/returns/%d/pdf", num(r["id"]))).expect(200).m()
		if !bytes.HasPrefix(e.sb.file(t, link["url"].(string)), []byte("%PDF")) {
			t.Fatal("credit note PDF")
		}
		admin.post(t, "/returns", map[string]any{"bill_id": copyID, "reason": "copy", "settlement": "adjust_ledger",
			"lines": []any{map[string]any{"bill_line_id": 1, "qty": "1", "unit_id": units["bag"]}}}).expect(409)
	})

	t.Run("purchase return and cancel", func(t *testing.T) {
		pu := admin.get(t, fmt.Sprintf("/purchases/%d", purchaseID)).expect(200).m()
		pl := num(pu["lines"].([]any)[0].(map[string]any)["id"])
		r := admin.post(t, "/purchase-returns", map[string]any{"purchase_id": purchaseID, "reason": "broken bags",
			"lines": []any{map[string]any{"purchase_line_id": pl, "qty": "10", "unit_id": units["bag"]}}}).expect(201).m()
		if !strings.HasPrefix(r["doc_no"].(string), "PR/") {
			t.Fatalf("purchase return %v", r)
		}
		// A purchase whose stock was sold cannot be cancelled.
		p2 := admin.post(t, "/purchases", map[string]any{"supplier_id": dealer, "supplier_invoice_no": "SC/2",
			"supplier_invoice_dt": "2026-10-10", "location_id": shop,
			"lines": []any{map[string]any{"item_id": steel, "qty": "100", "unit_id": units["kg"], "rate": "62"}}}).expect(201).m()
		admin.post(t, "/bills", map[string]any{"bill_type": "cash", "status": "active", "customer": map[string]any{"walk_in_name": "W"},
			"place_of_supply": "29", "lines": []any{map[string]any{"item_id": steel, "location_id": shop, "qty": "100", "unit_id": units["kg"], "rate": "1"}},
			"payments": []any{map[string]any{"mode": "cash", "amount": "118"}}}).expect(201)
		owner.post(t, fmt.Sprintf("/purchases/%d/cancel", num(p2["id"])), map[string]any{"reason": "wrong dealer"}).expect(422).code("INSUFFICIENT_STOCK")
		p3 := admin.post(t, "/purchases", map[string]any{"supplier_id": dealer, "supplier_invoice_no": "SC/3",
			"supplier_invoice_dt": "2026-10-10", "location_id": yard,
			"lines": []any{map[string]any{"item_id": cement, "qty": "5", "unit_id": units["bag"], "rate": "320"}}}).expect(201).m()
		c := owner.post(t, fmt.Sprintf("/purchases/%d/cancel", num(p3["id"])), map[string]any{"reason": "entered twice"}).expect(200).m()
		if c["status"] != "cancelled" {
			t.Fatalf("purchase cancel %v", c)
		}
		admin.get(t, "/purchases?q=SC&sort=-received_date").expect(200)
	})

	t.Run("transfers, adjustments, counts", func(t *testing.T) {
		tr := admin.post(t, "/stock-transfers", map[string]any{"from_location_id": godown1, "to_location_id": shop,
			"lines": []any{map[string]any{"item_id": cement, "qty": "20", "unit_id": units["bag"]}}}).expect(201).m()
		if stockAt(cement, "Shop") != "20.000" {
			t.Fatalf("shop after transfer %s", stockAt(cement, "Shop"))
		}
		admin.get(t, fmt.Sprintf("/stock-transfers/%d", num(tr["id"]))).expect(200)
		admin.get(t, "/stock-transfers?location_id="+fmt.Sprint(shop)).expect(200)
		admin.post(t, "/stock-transfers", map[string]any{"from_location_id": shop, "to_location_id": godown1,
			"lines": []any{map[string]any{"item_id": cement, "qty": "999", "unit_id": units["bag"]}}}).expect(422).code("INSUFFICIENT_STOCK")
		admin.post(t, "/stock-adjustments", map[string]any{"location_id": shop, "reason": "damage",
			"lines": []any{map[string]any{"item_id": cement, "qty_change": "-1"}}}).expect(201)
		admin.post(t, "/stock-adjustments", map[string]any{"location_id": shop, "reason": "other",
			"lines": []any{map[string]any{"item_id": cement, "qty_change": "1"}}}).expect(400)
		admin.get(t, "/stock-adjustments").expect(200)
		pv := admin.post(t, "/stock-counts/preview", map[string]any{"location_id": shop,
			"lines": []any{map[string]any{"item_id": cement, "counted_qty": "18"}}}).expect(200).m()
		if pv["lines"].([]any)[0].(map[string]any)["difference"] != "-1.000" {
			t.Fatalf("count preview %v", pv)
		}
		admin.post(t, "/stock-counts", map[string]any{"location_id": shop,
			"lines": []any{map[string]any{"item_id": cement, "counted_qty": "18"}}}).expect(201)
		if stockAt(cement, "Shop") != "18.000" {
			t.Fatalf("shop after count %s", stockAt(cement, "Shop"))
		}
	})

	t.Run("item import", func(t *testing.T) {
		f := excelize.NewFile()
		_ = f.SetSheetRow("Sheet1", "A1", &[]any{"Name", "Brand *", "Grade *", "HSN code *", "GST rate % *", "Base unit *", "Reorder level", "SKU", "Opening qty - Godown 1", "Opening cost per base unit"})
		_ = f.SetSheetRow("Sheet1", "A2", &[]any{"", "ACC", "PPC", "2523", "18", "bag", "10", "ACC-PPC", "50", "320"})
		_ = f.SetSheetRow("Sheet1", "A3", &[]any{"", "Ramco", "OPC 53", "2523", "18", "bag", "", "", "", ""})
		buf, _ := f.WriteToBuffer()
		job := admin.upload(t, http.MethodPost, "/items/import", map[string][]string{"category_id": {fmt.Sprint(cementCat)}}, "file", "items.xlsx", buf.Bytes()).expect(202).m()
		eventually(t, "item import", func() bool {
			j := admin.get(t, "/jobs/"+job["id"].(string)).expect(200).m()
			if j["status"] == "failed" {
				t.Fatalf("import failed %v", j)
			}
			return j["status"] == "succeeded"
		})
		if num(get(admin.get(t, "/items?q=ramco").expect(200).m(), "meta", "total")) != 1 {
			t.Fatal("imported item missing")
		}
	})

	t.Run("dashboard and reports", func(t *testing.T) {
		s := admin.get(t, "/dashboard/summary").expect(200).m()
		if num(get(s, "sales", "bill_count")) == 0 {
			t.Fatalf("summary %v", s["sales"])
		}
		admin.get(t, "/dashboard/sales-trend?bucket=day").expect(200)
		admin.get(t, "/dashboard/categories").expect(200)
		admin.get(t, "/dashboard/top-items?kind=slow&limit=5").expect(200)
		admin.get(t, "/dashboard/low-stock").expect(200)
		p := admin.get(t, "/dashboard/pending").expect(200).m()
		if num(p["blocked_bills_today"]) < 1 {
			t.Fatalf("pending %v", p)
		}
		for _, key := range []string{"sales-register", "bill-type-register", "item-sales", "category-sales", "gst-b2b", "gst-b2c",
			"gst-hsn-summary", "gst-credit-notes", "einvoice-status", "purchase-register", "input-gst", "stock-summary",
			"stock-movement", "transfers", "adjustments", "receivables-ageing", "payables-ageing", "collections",
			"payments-made", "profit", "edits-cancellations"} {
			r := admin.get(t, "/reports/"+key+"?from=2026-04-01&to=2027-03-31").expect(200).m()
			if len(r["columns"].([]any)) == 0 {
				t.Fatalf("report %s has no columns", key)
			}
		}
		sr := admin.get(t, "/reports/sales-register?from=2026-04-01&to=2027-03-31").expect(200).m()
		if num(get(sr, "meta", "total")) == 0 {
			t.Fatal("sales register empty")
		}
		x := admin.get(t, "/reports/gst-hsn-summary?format=xlsx&from=2026-04-01&to=2027-03-31").expect(200).m()
		if !bytes.HasPrefix(e.sb.file(t, x["url"].(string)), []byte("PK")) {
			t.Fatal("xlsx export")
		}
		pdf := admin.get(t, "/reports/stock-summary?format=pdf&from=2026-04-01&to=2027-03-31").expect(200).m()
		if !bytes.HasPrefix(e.sb.file(t, pdf["url"].(string)), []byte("%PDF")) {
			t.Fatal("pdf export")
		}
	})

	t.Run("notifications, search, lookups, audit", func(t *testing.T) {
		n := owner.get(t, "/notifications").expect(200).m()
		first := num(n["data"].([]any)[0].(map[string]any)["id"])
		owner.post(t, fmt.Sprintf("/notifications/%d/read", first), nil).expect(204)
		owner.post(t, "/notifications/read-all", nil).expect(204)
		if num(get(owner.get(t, "/notifications?unread_only=true").expect(200).m(), "meta", "total")) != 0 {
			t.Fatal("still unread")
		}
		sr := admin.get(t, "/search?q=ravi").expect(200).m()
		if len(sr["parties"].([]any)) != 1 {
			t.Fatalf("search %v", sr)
		}
		var states []map[string]any
		admin.get(t, "/lookups/states").expect(200).into(&states)
		if len(states) < 36 {
			t.Fatal("states")
		}
		admin.get(t, "/audit-log").expect(403)
		a := owner.get(t, "/audit-log?entity=bill").expect(200).m()
		if num(get(a, "meta", "total")) == 0 {
			t.Fatal("no bill audit entries")
		}
	})

	t.Run("tally export and import", func(t *testing.T) {
		j := admin.post(t, "/tally/exports", map[string]any{"from": "2026-04-01", "to": "2027-03-31",
			"scope": []string{"masters", "sales", "credit_notes", "receipts", "purchases", "payments"}}).expect(202).m()
		var job map[string]any
		eventually(t, "tally export", func() bool {
			job = admin.get(t, "/jobs/"+j["id"].(string)).expect(200).m()
			if job["status"] == "failed" {
				t.Fatalf("export failed %v", job)
			}
			return job["status"] == "succeeded"
		})
		xmlData := e.sb.file(t, job["result_url"].(string))
		if !bytes.Contains(xmlData, []byte("<VOUCHER")) || !bytes.Contains(xmlData, []byte("<LEDGER")) {
			t.Fatal("export has no vouchers")
		}
		if p := admin.get(t, "/dashboard/pending").expect(200).m(); num(p["tally_unexported"]) != 0 {
			t.Fatalf("unexported after export %v", p["tally_unexported"])
		}
		// Second run exports nothing new.
		j2 := admin.post(t, "/tally/exports", map[string]any{"from": "2026-04-01", "to": "2027-03-31", "scope": []string{"sales"}}).expect(202).m()
		eventually(t, "second export", func() bool {
			return admin.get(t, "/jobs/"+j2["id"].(string)).expect(200).m()["status"] == "succeeded"
		})
		admin.get(t, "/tally/syncs").expect(200)

		sample := `<ENVELOPE><BODY><IMPORTDATA><REQUESTDATA>
<TALLYMESSAGE><STOCKGROUP NAME="Bricks"/></TALLYMESSAGE>
<TALLYMESSAGE><UNIT NAME="nos"><DECIMALPLACES>0</DECIMALPLACES></UNIT></TALLYMESSAGE>
<TALLYMESSAGE><STOCKITEM NAME="Red Brick"><PARENT>Bricks</PARENT><BASEUNITS>nos</BASEUNITS>
  <GSTDETAILS.LIST><HSNCODE>6901</HSNCODE><STATEWISEDETAILS.LIST><RATEDETAILS.LIST><GSTRATE>2.5</GSTRATE></RATEDETAILS.LIST><RATEDETAILS.LIST><GSTRATE>5</GSTRATE></RATEDETAILS.LIST></STATEWISEDETAILS.LIST></GSTDETAILS.LIST>
  <BATCHALLOCATIONS.LIST><GODOWNNAME>Main Location</GODOWNNAME><OPENINGBALANCE> 1000 nos</OPENINGBALANCE><OPENINGRATE>8.50/nos</OPENINGRATE></BATCHALLOCATIONS.LIST>
</STOCKITEM></TALLYMESSAGE>
<TALLYMESSAGE><LEDGER NAME="Old Customer"><PARENT>Sundry Debtors</PARENT><OPENINGBALANCE>-2500.00</OPENINGBALANCE><LEDSTATENAME>Karnataka</LEDSTATENAME></LEDGER></TALLYMESSAGE>
<TALLYMESSAGE><LEDGER NAME="Old Dealer"><PARENT>Sundry Creditors</PARENT><OPENINGBALANCE>4000.00</OPENINGBALANCE></LEDGER></TALLYMESSAGE>
</REQUESTDATA></IMPORTDATA></BODY></ENVELOPE>`
		admin.upload(t, http.MethodPost, "/tally/imports", map[string][]string{"cutover_date": {"2026-04-01"}}, "file", "tally.xml", []byte(sample)).expect(403)
		pv := owner.upload(t, http.MethodPost, "/tally/imports", map[string][]string{"cutover_date": {"2026-04-01"}}, "file", "tally.xml", []byte(sample)).expect(201).m()
		if len(pv["issues"].([]any)) == 0 { // unknown godown "Main Location"
			t.Fatalf("expected an issue for the godown: %v", pv)
		}
		cj := owner.post(t, fmt.Sprintf("/tally/imports/%d/commit", num(pv["import_id"])), map[string]any{
			"location_map": map[string]any{"Main Location": godown1}}).expect(202).m()
		eventually(t, "tally import", func() bool {
			j := owner.get(t, "/jobs/"+cj["id"].(string)).expect(200).m()
			if j["status"] == "failed" {
				t.Fatalf("import failed %v", j)
			}
			return j["status"] == "succeeded"
		})
		owner.post(t, fmt.Sprintf("/tally/imports/%d/commit", num(pv["import_id"])), nil).expect(409)
		brick := admin.get(t, "/items?q=red%20brick").expect(200).m()
		if num(get(brick, "meta", "total")) != 1 {
			t.Fatal("brick not imported")
		}
		bi := brick["data"].([]any)[0].(map[string]any)
		if bi["stock_total"] != "1000.000" || bi["gst_rate"] != "5" {
			t.Fatalf("brick %v", bi)
		}
		parties := admin.get(t, "/parties?q=old").expect(200).m()
		for _, p := range parties["data"].([]any) {
			pm := p.(map[string]any)
			want := map[string]string{"Old Customer": "2500.00", "Old Dealer": "-4000.00"}[pm["name"].(string)]
			if pm["balance"] != want {
				t.Fatalf("%s balance %v want %s", pm["name"], pm["balance"], want)
			}
		}
	})

	t.Run("deactivate masters", func(t *testing.T) {
		admin.post(t, fmt.Sprintf("/items/%d/deactivate", steel), nil).expect(403)
		owner.post(t, fmt.Sprintf("/categories/%d/deactivate", steelCat), nil).expect(409)
		owner.post(t, fmt.Sprintf("/items/%d/deactivate", steel), nil).expect(204)
		owner.post(t, fmt.Sprintf("/categories/%d/deactivate", steelCat), nil).expect(204)
		owner.post(t, fmt.Sprintf("/parties/%d/deactivate", kumar), nil).expect(204)
		admin.post(t, "/bills", cashBill(godown1, map[string]any{"party_id": kumar}, nil)).expect(400)
	})

	t.Run("remaining endpoints", func(t *testing.T) {
		anon := &client{e: e}
		if h := anon.get(t, "/health").expect(200).m(); h["status"] != "ok" {
			t.Fatalf("health %v", h)
		}
		if s := admin.get(t, "/settings").expect(200).m(); s["shop_name"] != "Demo Building Materials" {
			t.Fatalf("settings %v", s)
		}
		c := admin.get(t, fmt.Sprintf("/categories/%d", cementCat)).expect(200).m()
		admin.patch(t, fmt.Sprintf("/categories/%d", cementCat), map[string]any{"name": "Cement", "default_hsn": "2523",
			"description": "OPC and PPC", "default_gst": c["default_gst"]}).expect(200)
		var ul []map[string]any
		admin.get(t, "/units").expect(200).into(&ul)
		for _, u := range ul {
			units[u["symbol"].(string)] = num(u["id"])
		}
		admin.patch(t, fmt.Sprintf("/units/%d", units["pc"]), map[string]any{"name": "Piece", "symbol": "pc", "uqc_code": "PCS"}).expect(200)
		p := admin.patch(t, fmt.Sprintf("/parties/%d", ravi), map[string]any{"kind": "customer", "name": "Ravi Builders Pvt Ltd",
			"gstin": "29AAAPR1234C1Z5", "credit_limit": "600000"}).expect(200).m()
		if p["credit_limit"] != "600000.00" || len(p["sites"].([]any)) != 1 {
			t.Fatalf("party patch %v", p)
		}
		pays := admin.get(t, "/payments").expect(200).m()
		admin.get(t, fmt.Sprintf("/payments/%d", num(pays["data"].([]any)[0].(map[string]any)["id"]))).expect(200)

		// A B2B bill saved while e-invoicing was off gets its IRN on request.
		owner.put(t, "/settings", map[string]any{"shop_name": "Demo Building Materials", "address": "1 Market Road, Bengaluru 560001",
			"state_code": "29", "gstin": "29ABCDE1234F1Z5", "einvoice_enabled": false}).expect(200)
		b := admin.post(t, "/bills", cashBill(godown1, map[string]any{"party_id": ravi}, nil)).expect(201).m()
		if get(b, "einvoice", "status") != "not_required" {
			t.Fatalf("einvoice %v", b["einvoice"])
		}
		admin.post(t, fmt.Sprintf("/bills/%d/einvoice", num(b["id"])), nil).expect(409) // turned off
		owner.put(t, "/settings", map[string]any{"shop_name": "Demo Building Materials", "address": "1 Market Road, Bengaluru 560001",
			"state_code": "29", "gstin": "29ABCDE1234F1Z5", "einvoice_enabled": true}).expect(200)
		admin.post(t, fmt.Sprintf("/bills/%d/einvoice", num(b["id"])), nil).expect(202)
		eventually(t, "requested IRN", func() bool {
			return get(admin.get(t, fmt.Sprintf("/bills/%d", num(b["id"]))).expect(200).m(), "einvoice", "status") == "generated"
		})
		admin.post(t, fmt.Sprintf("/bills/%d/einvoice", num(b["id"])), nil).expect(409) // already generated
	})

	t.Run("operation coverage", func(t *testing.T) {
		missing := e.contract.uncovered()
		t.Logf("%d spec operations not exercised: %v", len(missing), missing)
	})

	t.Run("stock reconciles with movements", func(t *testing.T) {
		var n int
		if err := e.super.QueryRow(context.Background(), `select count(*) from app.stock_balance sb
			left join (select item_id, location_id, sum(qty_in - qty_out) q from app.stock_movement group by 1, 2) m
			using (item_id, location_id) where sb.qty <> coalesce(m.q, 0)`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%d stock rows disagree with movements", n)
		}
	})
}
