// Builds postman/billing-api.postman_collection.json: every operation in
// openapi.yaml with a realistic body, in the order of a working day, so the
// Collection Runner can go through the whole API against a dev database.
// Ids created along the way are stored in collection variables.
//
//   node postman/build-collection.mjs
//
// The check at the end fails if an operation in openapi.yaml is missing here.

import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));

// ---------------------------------------------------------------- helpers

const ok = (...codes) => `pm.test("status ${codes.join(" or ")}", () => pm.expect(pm.response.code).to.be.oneOf([${codes.join(", ")}]));`;
const save = (name, expr) => `try { const j = pm.response.json(); const v = ${expr}; if (v !== undefined && v !== null) pm.collectionVariables.set("${name}", v); } catch (e) {}`;
const etag = (name) => `const t = pm.response.headers.get("ETag"); if (t) pm.collectionVariables.set("${name}", t.replace(/"/g, ""));`;

function req(name, method, path, { body, tests = [ok(200)], desc = "", headers = [], form, auth } = {}) {
  const segs = path.replace(/^\//, "").split("/");
  const h = [...headers];
  if (body !== undefined) h.push({ key: "Content-Type", value: "application/json" });
  if (method === "POST") h.push({ key: "Idempotency-Key", value: "{{$guid}}", description: "New UUID per action; reuse it only when retrying the same request." });
  const r = {
    name,
    event: [{ listen: "test", script: { type: "text/javascript", exec: tests.flatMap((t) => t.split("\n")) } }],
    request: {
      method,
      header: h,
      url: { raw: "{{baseUrl}}/" + segs.join("/"), host: ["{{baseUrl}}"], path: segs },
      description: desc,
    },
  };
  if (auth) r.request.auth = auth;
  if (body !== undefined) r.request.body = { mode: "raw", raw: JSON.stringify(body, null, 2), options: { raw: { language: "json" } } };
  if (form) r.request.body = { mode: "formdata", formdata: form };
  const [p, q] = path.split("?");
  r.request.url.path = p.replace(/^\//, "").split("/");
  r.request.url.raw = "{{baseUrl}}" + path;
  if (q) r.request.url.query = q.split("&").map((kv) => { const [key, value] = kv.split("="); return { key, value }; });
  r._op = method + " " + p.replace(/\{\{[a-z_0-9]+\}\}/g, "{}");
  return r;
}

const folder = (name, description, items) => ({ name, description, item: items });
const noauth = { type: "noauth" };
const ifMatch = (v) => [{ key: "If-Match", value: `"{{${v}}}"`, description: "Bill version from the last GET (ETag)." }];
const SA = "\n\nSuper Admin only: run with the Super Admin's token.";

// ---------------------------------------------------------------- requests

const supabaseHeaders = [{ key: "apikey", value: "{{supabase_anon_key}}" }, { key: "Content-Type", value: "application/json" }];
const auth = folder("Auth (Supabase)", "These call Supabase Auth directly, as the web app does with supabase-js. Public sign-up is disabled.", [
  {
    name: "Sign in (password)",
    event: [{ listen: "test", script: { type: "text/javascript", exec: [
      ok(200),
      "const j = pm.response.json();",
      "pm.environment.set('access_token', j.access_token);",
      "pm.environment.set('refresh_token', j.refresh_token);",
      "pm.collectionVariables.set('run', String(Date.now()).slice(-6));",
      "// A random valid-format GSTIN per run (GSTIN is unique per party).",
      "const L = () => String.fromCharCode(65 + Math.floor(Math.random() * 26));",
      "const D = () => String(Math.floor(Math.random() * 10));",
      "pm.collectionVariables.set('customer_gstin', '29' + L()+L()+L()+L()+L() + D()+D()+D()+D() + L() + '1Z' + D());",
      "pm.collectionVariables.set('supplier_gstin', '29' + L()+L()+L()+L()+L() + D()+D()+D()+D() + L() + '1Z' + D());",
    ] } }],
    request: { auth: noauth, method: "POST", header: supabaseHeaders,
      body: { mode: "raw", raw: JSON.stringify({ email: "{{email}}", password: "{{password}}" }, null, 2) },
      url: { raw: "{{supabase_url}}/auth/v1/token?grant_type=password", host: ["{{supabase_url}}"], path: ["auth", "v1", "token"], query: [{ key: "grant_type", value: "password" }] },
      description: "Stores access_token (1 hour) and refresh_token. Run this first, as the Super Admin." },
  },
  {
    name: "Refresh token",
    event: [{ listen: "test", script: { type: "text/javascript", exec: [ok(200), "const j = pm.response.json();", "pm.environment.set('access_token', j.access_token);", "pm.environment.set('refresh_token', j.refresh_token);"] } }],
    request: { auth: noauth, method: "POST", header: supabaseHeaders, body: { mode: "raw", raw: JSON.stringify({ refresh_token: "{{refresh_token}}" }, null, 2) },
      url: { raw: "{{supabase_url}}/auth/v1/token?grant_type=refresh_token", host: ["{{supabase_url}}"], path: ["auth", "v1", "token"], query: [{ key: "grant_type", value: "refresh_token" }] } },
  },
  {
    name: "Forgot password",
    event: [{ listen: "test", script: { type: "text/javascript", exec: [ok(200)] } }],
    request: { auth: noauth, method: "POST", header: supabaseHeaders, body: { mode: "raw", raw: JSON.stringify({ email: "{{email}}" }, null, 2) },
      url: { raw: "{{supabase_url}}/auth/v1/recover", host: ["{{supabase_url}}"], path: ["auth", "v1", "recover"] } },
  },
  {
    name: "Sign out",
    event: [{ listen: "test", script: { type: "text/javascript", exec: [ok(204, 200)] } }],
    request: { method: "POST", header: [{ key: "apikey", value: "{{supabase_anon_key}}" }],
      url: { raw: "{{supabase_url}}/auth/v1/logout", host: ["{{supabase_url}}"], path: ["auth", "v1", "logout"] },
      description: "Revokes the refresh token. The access token stays valid until it expires; deactivation is enforced by the API on every call." },
  },
]);

const line = (item, loc, qty, unit, rate, extra = {}) => ({ item_id: `{{${item}}}`, location_id: `{{${loc}}}`, qty, unit_id: `{{${unit}}}`, rate, ...extra });
const num = (v) => v; // ids are sent as numbers after variable substitution (see fixIds)

const folders = [
  folder("System", "", [
    req("Health", "GET", "/health", { auth: noauth, desc: "No auth. Checks the database." }),
    req("Me", "GET", "/me", { desc: "Current user, role and permission keys. Also records the login in the audit log." }),
    req("GST states", "GET", "/lookups/states"),
    req("Global search", "GET", "/search?q=cement"),
  ]),
  folder("Settings", "Super Admin for changes; everyone can read.", [
    req("Update shop settings", "PUT", "/settings", { desc: "Required before the first bill (state code decides CGST/SGST vs IGST)." + SA, body: {
      shop_name: "Demo Building Materials", address: "1 Market Road, Bengaluru 560001", state_code: "29", gstin: "29ABCDE1234F1Z5",
      phone: "9800000000", bank_name: "State Bank of India", bank_account_no: "123456789", bank_ifsc: "SBIN0000001", upi_id: "shop@upi",
      invoice_terms: "Goods once sold will not be taken back without approval.", prices_include_gst: false, default_charge_gst: "18",
      einvoice_enabled: true, number_prefixes: { cash_bill: "C", account_bill: "A", credit_note: "CN" },
      notification_channels: { email: true, whatsapp: false } } }),
    req("Get shop settings", "GET", "/settings"),
    req("Upload logo", "PUT", "/settings/logo", { desc: "Sends samples/logo.png (run with --working-dir postman). Any PNG/JPG up to 1 MB." + SA,
      form: [{ key: "file", type: "file", src: "samples/logo.png" }] }),
    req("Store e-invoice credentials", "PUT", "/settings/einvoice-credentials", { tests: [ok(204)],
      desc: "Provider \"mock\" simulates the IRP until a GSP/ASP is chosen (O-07). Needs APP_ENCRYPTION_KEY on the server." + SA,
      body: { provider: "mock", environment: "sandbox", username: "gsp-user", password: "gsp-password" } }),
    req("Test e-invoice login", "POST", "/settings/einvoice-credentials/test", { desc: SA.trim() }),
    req("Update Tally mapping", "PUT", "/settings/tally-mapping", { desc: SA.trim(), body: {
      sales_ledger_cash: "Sales", sales_ledger_account: "Sales - Account", cgst_ledger: "CGST", sgst_ledger: "SGST", igst_ledger: "IGST",
      labour_ledger: "Labour Charges", transport_ledger: "Transport Charges", round_off_ledger: "Round Off", cash_ledger: "Cash",
      bank_ledger: "Bank", purchase_ledger: "Purchase", voucher_type_cash: "Sales", voucher_type_account: "Sales" } }),
    req("Get Tally mapping", "GET", "/settings/tally-mapping"),
  ]),
  folder("Users", "Super Admin only.", [
    req("List users", "GET", "/users"),
    req("Invite user", "POST", "/users", { tests: [ok(201), save("invited_user_id", "j.id")],
      desc: "Creates the Supabase Auth user (invite email) and the profile.",
      body: { full_name: "Asha Admin", email: "asha+{{run}}@example.com", mobile: "98{{run}}00", role: "admin" } }),
    req("Update user", "PATCH", "/users/{{invited_user_id}}", { body: { full_name: "Asha (Admin)" } }),
    req("Deactivate user", "POST", "/users/{{invited_user_id}}/deactivate", { tests: [ok(204)], desc: "Blocks the user's next API call. The last Super Admin cannot be deactivated." }),
    req("Reactivate user", "POST", "/users/{{invited_user_id}}/reactivate", { tests: [ok(204)] }),
    req("Send password reset", "POST", "/users/{{invited_user_id}}/password-reset", { tests: [ok(204)] }),
  ]),
  folder("Locations", "", [
    req("List locations", "GET", "/locations", { tests: [ok(200),
      "const j = pm.response.json();",
      "j.forEach(l => { if (l.name === 'Shop') pm.collectionVariables.set('shop_id', l.id); if (l.name === 'Godown 1') pm.collectionVariables.set('godown1_id', l.id); });"] }),
    req("Add location", "POST", "/locations", { tests: [ok(201), save("location_id", "j.id")], desc: SA.trim(), body: { name: "Yard {{run}}", kind: "godown", address: "Hoskote" } }),
    req("Update location", "PATCH", "/locations/{{location_id}}", { desc: SA.trim(), body: { name: "Yard {{run}}", kind: "godown", address: "Hoskote Road" } }),
  ]),
  folder("Catalogue", "Units, categories and their attributes (category setup wizard, FR-IN-01/02).", [
    req("List units", "GET", "/units", { tests: [ok(200),
      "pm.response.json().forEach(u => { if (['bag','kg','ton','nos'].includes(u.symbol)) pm.collectionVariables.set('unit_' + u.symbol, u.id); });"] }),
    req("Create unit", "POST", "/units", { tests: [ok(201), save("unit_id", "j.id")], body: { name: "Box {{run}}", symbol: "box{{run}}", uqc_code: "BOX", decimals: 0 } }),
    req("Rename unit", "PATCH", "/units/{{unit_id}}", { body: { name: "Carton {{run}}", symbol: "box{{run}}", uqc_code: "BOX", decimals: 0 } }),
    req("Create category (wizard)", "POST", "/categories", { tests: [ok(201), save("category_id", "j.id")], body: {
      name: "Cement {{run}}", default_hsn: "2523", default_gst: "18", description: "OPC and PPC cement",
      units: [{ unit_id: "{{unit_bag}}", is_default: true }],
      attributes: [{ code: "brand", label: "Brand", data_type: "text", is_required: true },
                   { code: "grade", label: "Grade", data_type: "select", options: ["OPC 43", "OPC 53", "PPC"], is_required: true }] } }),
    req("Create category with a new unit", "POST", "/categories", { tests: [ok(201), save("steel_category_id", "j.id")], body: {
      name: "Steel {{run}}", default_hsn: "7214", default_gst: "18",
      units: [{ unit_id: "{{unit_kg}}", is_default: true }, { unit_id: "{{unit_ton}}" }, { new_unit: { name: "Rod {{run}}", symbol: "rod{{run}}", decimals: 0 } }],
      attributes: [{ code: "brand", label: "Brand", data_type: "text", is_required: true },
                   { code: "size", label: "Size", data_type: "number", value_unit: "mm", is_required: true }] } }),
    req("List categories", "GET", "/categories"),
    req("Get category", "GET", "/categories/{{category_id}}"),
    req("Update category", "PATCH", "/categories/{{category_id}}", { body: { name: "Cement {{run}}", default_hsn: "2523", default_gst: "18", description: "All cement" } }),
    req("Add attribute", "POST", "/categories/{{category_id}}/attributes", { tests: [ok(201), save("attribute_id", "j.id")],
      body: { code: "pack", label: "Pack", data_type: "text", is_required: false, part_of_name: false } }),
    req("Update attribute", "PATCH", "/categories/{{category_id}}/attributes/{{attribute_id}}", { body: { code: "pack", label: "Pack size", data_type: "text" } }),
    req("Delete attribute", "DELETE", "/categories/{{category_id}}/attributes/{{attribute_id}}", { tests: [ok(204)], desc: "Only when no item uses it." + SA }),
    req("Excel import template", "GET", "/categories/{{category_id}}/import-template", { desc: "Downloads an .xlsx whose columns follow the category's attributes." }),
    req("Create a throwaway category", "POST", "/categories", { tests: [ok(201), save("temp_category_id", "j.id")], body: { name: "Temp {{run}}" } }),
    req("Deactivate category", "POST", "/categories/{{temp_category_id}}/deactivate", { tests: [ok(204)], desc: "Only when it has no active items." + SA }),
  ]),
  folder("Items", "", [
    req("Create item with opening stock", "POST", "/items", { tests: [ok(201), save("item_id", "j.id")],
      desc: "Name is built from the attributes when omitted (\"UltraTech ... OPC 53\"). Duplicate category + attributes = 409 DUPLICATE.",
      body: { category_id: "{{category_id}}", attributes: { brand: "UltraTech {{run}}", grade: "OPC 53" }, hsn_code: "2523", gst_rate: "18",
        base_unit_id: "{{unit_bag}}", reorder_level: "50", opening_stock: [{ location_id: "{{godown1_id}}", qty: "500", unit_cost: "350" }] } }),
    req("Create item with an alternate unit", "POST", "/items", { tests: [ok(201), save("steel_id", "j.id")],
      body: { category_id: "{{steel_category_id}}", attributes: { brand: "JSW {{run}}", size: 12 }, hsn_code: "7214", gst_rate: "18",
        base_unit_id: "{{unit_kg}}", units: [{ unit_id: "{{unit_ton}}", factor_to_base: "1000" }], reorder_level: "500",
        opening_stock: [{ location_id: "{{godown1_id}}", qty: "5000", unit_cost: "60" }] } }),
    req("List / search items", "GET", "/items?q=ultratech&stock_status=ok&sort=-stock_value&page=1&page_size=50", { desc: "Attribute filters: attr[brand]=Tata&attr[size]=12" }),
    req("Get item", "GET", "/items/{{item_id}}"),
    req("Update item", "PATCH", "/items/{{item_id}}", { desc: "Unit factors are frozen once the item has stock movements (FR-IN-07).",
      body: { category_id: "{{category_id}}", attributes: { brand: "UltraTech {{run}}", grade: "OPC 53" }, hsn_code: "2523", gst_rate: "18",
        base_unit_id: "{{unit_bag}}", reorder_level: "60" } }),
    req("Item stock ledger", "GET", "/items/{{item_id}}/movements?location_id={{godown1_id}}"),
    req("Price hints", "GET", "/items/{{steel_id}}/price-hints?unit_id={{unit_ton}}"),
    req("Bulk import items (Excel)", "POST", "/items/import", { tests: [ok(202), save("job_id", "j.id")],
      desc: "Sends samples/items-import.xlsx as a dry run (nothing written; the job result lists row errors). Poll System > Job status.",
      form: [{ key: "file", type: "file", src: "samples/items-import.xlsx" }, { key: "category_id", value: "{{category_id}}", type: "text" },
        { key: "dry_run", value: "true", type: "text" }] }),
    req("Job status", "GET", "/jobs/{{job_id}}"),
    req("Create a throwaway item", "POST", "/items", { tests: [ok(201), save("temp_item_id", "j.id")],
      body: { category_id: "{{category_id}}", attributes: { brand: "Temp {{run}}", grade: "PPC" }, hsn_code: "2523", gst_rate: "18", base_unit_id: "{{unit_bag}}" } }),
    req("Deactivate item", "POST", "/items/{{temp_item_id}}/deactivate", { tests: [ok(204)], desc: SA.trim() }),
  ]),
  folder("Parties", "Customers and dealers.", [
    req("Create B2B customer", "POST", "/parties", { tests: [ok(201), save("customer_id", "j.id")],
      body: { kind: "customer", name: "Ravi Builders {{run}}", mobile: "9845012345", gstin: "{{customer_gstin}}", state_code: "29",
        billing_address: "Yelahanka, Bengaluru 560064", credit_limit: "500000", sites: [{ label: "Plot 21", address: "Plot 21, Yelahanka" }] } }),
    req("Create dealer", "POST", "/parties", { tests: [ok(201), save("supplier_id", "j.id")],
      body: { kind: "supplier", name: "Southern Cement {{run}}", gstin: "{{supplier_gstin}}", billing_address: "Peenya, Bengaluru" } }),
    req("List parties", "GET", "/parties?kind=customer&with_balance=any&sort=-balance"),
    req("Get party", "GET", "/parties/{{customer_id}}"),
    req("Update party", "PATCH", "/parties/{{customer_id}}", { body: { kind: "customer", name: "Ravi Builders {{run}}", gstin: "{{customer_gstin}}", credit_limit: "600000" } }),
    req("Party ledger", "GET", "/parties/{{customer_id}}/ledger?from=2026-04-01&to=2027-03-31"),
    req("Statement PDF", "GET", "/parties/{{customer_id}}/statement?from=2026-04-01&to=2027-03-31"),
    req("Create a throwaway party", "POST", "/parties", { tests: [ok(201), save("temp_party_id", "j.id")], body: { kind: "customer", name: "Temp {{run}}" } }),
    req("Deactivate party", "POST", "/parties/{{temp_party_id}}/deactivate", { tests: [ok(204)], desc: SA.trim() }),
  ]),
  folder("Purchases", "", [
    req("Record purchase", "POST", "/purchases", { tests: [ok(201), save("purchase_id", "j.id"), save("purchase_line_id", "j.lines[0].id")],
      desc: "Adds stock at the location, recalculates average cost (freight apportioned), credits the dealer. Same dealer invoice twice = 409 DUPLICATE.",
      body: { supplier_id: "{{supplier_id}}", supplier_invoice_no: "SC/{{run}}", supplier_invoice_dt: "2026-10-09", location_id: "{{godown1_id}}",
        freight: "1000", lines: [{ item_id: "{{item_id}}", qty: "100", unit_id: "{{unit_bag}}", rate: "300", gst_rate: "18" }],
        payment: { mode: "bank_transfer", amount: "10000", reference: "UTR{{run}}" } } }),
    req("List purchases", "GET", "/purchases?party_id={{supplier_id}}"),
    req("Get purchase", "GET", "/purchases/{{purchase_id}}"),
    req("Return to dealer", "POST", "/purchase-returns", { tests: [ok(201)],
      body: { purchase_id: "{{purchase_id}}", reason: "Broken bags", lines: [{ purchase_line_id: "{{purchase_line_id}}", qty: "5", unit_id: "{{unit_bag}}" }] } }),
    req("Record a purchase to cancel", "POST", "/purchases", { tests: [ok(201), save("purchase2_id", "j.id")],
      body: { supplier_id: "{{supplier_id}}", supplier_invoice_no: "SC/{{run}}/2", supplier_invoice_dt: "2026-10-09", location_id: "{{godown1_id}}",
        lines: [{ item_id: "{{item_id}}", qty: "10", unit_id: "{{unit_bag}}", rate: "310" }] } }),
    req("Cancel purchase", "POST", "/purchases/{{purchase2_id}}/cancel", { desc: "Only while its stock is unsold; otherwise 422 INSUFFICIENT_STOCK." + SA, body: { reason: "Entered twice" } }),
  ]),
  folder("Bills", "Cash bills deduct stock at each line's location; Account bills never change stock.", [
    req("Calculate (live preview)", "POST", "/bills/calculate", { desc: "Nothing is saved. Shows tax, totals, stock warnings and credit warning.",
      body: { bill_type: "cash", status: "active", customer: { party_id: "{{customer_id}}" }, place_of_supply: "29",
        lines: [line("item_id", "godown1_id", "10", "unit_bag", "400"), line("steel_id", "godown1_id", "1.500", "unit_ton", "68500", { discount_pct: "2" })],
        charges: [{ kind: "labour", amount: "1200", gst_rate: "18" }, { kind: "transport", amount: "2500" }] } }),
    req("Create Cash bill (active)", "POST", "/bills", { tests: [ok(201), save("bill_id", "j.id"), save("bill_line_id", "j.lines[0].id"), etag("bill_version")],
      desc: "Numbers the bill, deducts stock, posts ledger and payments in one transaction. B2B bills queue the e-invoice. 422 INSUFFICIENT_STOCK lists short lines.",
      body: { bill_type: "cash", status: "active", bill_date: "2026-10-10", customer: { party_id: "{{customer_id}}", site_address: "Plot 21, Yelahanka" },
        place_of_supply: "29", vehicle_no: "KA05AB1234",
        lines: [line("item_id", "godown1_id", "10", "unit_bag", "400"), line("steel_id", "godown1_id", "1.500", "unit_ton", "68500", { discount_pct: "2" })],
        charges: [{ kind: "labour", amount: "1200", gst_rate: "18" }],
        payments: [{ mode: "upi", amount: "50000", reference: "UPI/628401" }] } }),
    req("Create a second Cash bill (for returns)", "POST", "/bills", { tests: [ok(201), save("bill2_id", "j.id"), save("bill2_line_id", "j.lines[0].id")],
      body: { bill_type: "cash", status: "active", customer: { party_id: "{{customer_id}}" }, place_of_supply: "29",
        lines: [line("item_id", "godown1_id", "20", "unit_bag", "395")] } }),
    req("Get bill", "GET", "/bills/{{bill_id}}", { tests: [ok(200), etag("bill_version")], desc: "ETag is the version; send it as If-Match when editing or cancelling." }),
    req("List bills", "GET", "/bills?bill_type=cash&status=active&payment_status=partial&sort=-bill_date"),
    req("Invoice PDF", "GET", "/bills/{{bill_id}}/pdf?copy=original", { desc: "409 while the B2B e-invoice is pending: print after the IRN arrives." }),
    req("WhatsApp share", "POST", "/bills/{{bill_id}}/share"),
    req("Generate / retry e-invoice", "POST", "/bills/{{bill2_id}}/einvoice", { tests: [ok(202, 409)], desc: "409 when the IRN is already generated or not required." }),
    req("Generate e-way bill", "POST", "/bills/{{bill2_id}}/ewaybill", { tests: [ok(202, 409)], desc: "B2B bills need the IRN first.",
      body: { vehicle_no: "KA05AB1234", transport_mode: "road", distance_km: 30 } }),
    req("Copy to Account bill", "POST", "/bills/{{bill_id}}/copy-to-account", { tests: [ok(201), save("copy_id", "j.id"), etag("copy_version")],
      desc: "The copy never changes stock and does not post to the ledger again (BR-04).", body: { status: "active" } }),
    req("Edit active bill", "PUT", "/bills/{{bill_id}}", { tests: [ok(200), etag("bill_version")], headers: ifMatch("bill_version"),
      desc: "Stores the old version, reverses and re-posts. 409 EINVOICE_WINDOW_CLOSED after 24 h from the IRN." + SA,
      body: { bill_type: "cash", status: "active", customer: { party_id: "{{customer_id}}" }, place_of_supply: "29",
        lines: [line("item_id", "godown1_id", "12", "unit_bag", "390")], payments: [{ mode: "upi", amount: "1000" }],
        reason: "Rate corrected after customer call" } }),
    req("Revision history", "GET", "/bills/{{bill_id}}/revisions"),
    req("Create draft Account bill", "POST", "/bills", { tests: [ok(201), save("draft_id", "j.id")],
      body: { bill_type: "account", status: "draft", customer: { party_id: "{{customer_id}}" }, place_of_supply: "29",
        lines: [{ item_id: "{{item_id}}", qty: "5", unit_id: "{{unit_bag}}", rate: "410" }] } }),
    req("Update draft", "PUT", "/bills/{{draft_id}}", { headers: [{ key: "If-Match", value: "\"1\"" }],
      body: { bill_type: "account", status: "draft", customer: { party_id: "{{customer_id}}" }, place_of_supply: "29",
        lines: [{ item_id: "{{item_id}}", qty: "6", unit_id: "{{unit_bag}}", rate: "410" }] } }),
    req("Finalize draft", "POST", "/bills/{{draft_id}}/finalize", { desc: "Numbers and posts the draft." }),
    req("Cancel e-invoice (IRN)", "POST", "/bills/{{draft_id}}/einvoice/cancel", { tests: [ok(202, 409)],
      desc: "Within 24 hours of generation. 409 if there is no IRN yet (the job may still be running)." + SA, body: { reason: "Wrong rate" } }),
    req("Create a draft to delete", "POST", "/bills", { tests: [ok(201), save("draft2_id", "j.id")],
      body: { bill_type: "cash", status: "draft", customer: { walk_in_name: "Walk-in customer" }, place_of_supply: "29",
        lines: [{ item_id: "{{item_id}}", qty: "1", unit_id: "{{unit_bag}}", rate: "400" }] } }),
    req("Delete draft", "DELETE", "/bills/{{draft2_id}}", { tests: [ok(204)], desc: "Active bills are cancelled, never deleted." }),
    req("Cancel the Account copy", "POST", "/bills/{{copy_id}}/cancel", { headers: ifMatch("copy_version"), desc: SA.trim(), body: { reason: "Not needed" } }),
  ]),
  folder("Payments", "", [
    req("Receipt with auto allocation", "POST", "/payments", { tests: [ok(201), save("payment_id", "j.id")],
      body: { direction: "received", party_id: "{{customer_id}}", mode: "upi", amount: "5000", reference: "UTR{{run}}", auto_allocate: true } }),
    req("Cheque receipt against a bill", "POST", "/payments", { tests: [ok(201), save("cheque_payment_id", "j.id")],
      body: { direction: "received", party_id: "{{customer_id}}", mode: "cheque", amount: "1000", cheque_no: "000123", cheque_date: "2026-10-10",
        bank_name: "HDFC", allocations: [{ ref_type: "cash_bill", ref_id: "{{bill2_id}}", amount: "1000" }] } }),
    req("List payments", "GET", "/payments?direction=received&from=2026-04-01&to=2027-03-31"),
    req("Get payment", "GET", "/payments/{{payment_id}}"),
    req("Cheque bounced", "POST", "/payments/{{cheque_payment_id}}/cheque-status", { desc: "Reverses the receipt in the ledger and reopens the bill.",
      body: { status: "bounced", date: "2026-10-12", notes: "Insufficient funds" } }),
    req("Receipt PDF", "GET", "/payments/{{payment_id}}/receipt"),
    req("Cancel payment", "POST", "/payments/{{payment_id}}/cancel", { desc: SA.trim(), body: { reason: "Entered twice" } }),
  ]),
  folder("Returns", "Sales returns and credit notes (pending until the Super Admin approves; auto-approved when the Super Admin creates them).", [
    req("Create return", "POST", "/returns", { tests: [ok(201), save("return_id", "j.id")],
      body: { bill_id: "{{bill2_id}}", reason: "Two bags damaged in transit", settlement: "adjust_ledger",
        lines: [{ bill_line_id: "{{bill2_line_id}}", qty: "2", unit_id: "{{unit_bag}}", location_id: "{{godown1_id}}", restock: true }] } }),
    req("List returns", "GET", "/returns?status=pending"),
    req("Get return", "GET", "/returns/{{return_id}}"),
    req("Approve return", "POST", "/returns/{{return_id}}/approve", { tests: [ok(200, 409)], desc: "409 if it was auto-approved." + SA }),
    req("Create return to reject", "POST", "/returns", { tests: [ok(201), save("return2_id", "j.id")],
      body: { bill_id: "{{bill2_id}}", reason: "Wrong item", settlement: "refund", refund: { mode: "cash", amount: "100" },
        lines: [{ bill_line_id: "{{bill2_line_id}}", qty: "1", unit_id: "{{unit_bag}}", restock: false }] } }),
    req("Reject return", "POST", "/returns/{{return2_id}}/reject", { tests: [ok(200, 409)], desc: SA.trim(), body: { reason: "Not our goods" } }),
    req("Credit note PDF", "GET", "/returns/{{return_id}}/pdf"),
  ]),
  folder("Stock", "Transfers, adjustments and physical counts.", [
    req("Transfer godown to shop", "POST", "/stock-transfers", { tests: [ok(201), save("transfer_id", "j.id")],
      body: { from_location_id: "{{godown1_id}}", to_location_id: "{{shop_id}}", vehicle_no: "KA05AB1234",
        lines: [{ item_id: "{{item_id}}", qty: "20", unit_id: "{{unit_bag}}" }] } }),
    req("List transfers", "GET", "/stock-transfers?location_id={{shop_id}}"),
    req("Get transfer", "GET", "/stock-transfers/{{transfer_id}}"),
    req("Adjust stock", "POST", "/stock-adjustments", { tests: [ok(201)],
      body: { location_id: "{{shop_id}}", reason: "damage", notes: "Bag torn", lines: [{ item_id: "{{item_id}}", qty_change: "-1" }] } }),
    req("List adjustments", "GET", "/stock-adjustments"),
    req("Preview physical count", "POST", "/stock-counts/preview", { body: { location_id: "{{shop_id}}", lines: [{ item_id: "{{item_id}}", counted_qty: "18" }] } }),
    req("Post physical count", "POST", "/stock-counts", { tests: [ok(201, 400)], desc: "400 when the count matches the system.",
      body: { location_id: "{{shop_id}}", lines: [{ item_id: "{{item_id}}", counted_qty: "18" }] } }),
  ]),
  folder("Dashboard", "", [
    req("Summary", "GET", "/dashboard/summary?from=2026-04-01&to=2027-03-31"),
    req("Sales trend", "GET", "/dashboard/sales-trend?bucket=month"),
    req("Category breakdown", "GET", "/dashboard/categories"),
    req("Top items", "GET", "/dashboard/top-items?kind=fast&limit=10"),
    req("Low stock", "GET", "/dashboard/low-stock"),
    req("Pending work", "GET", "/dashboard/pending"),
  ]),
  folder("Reports", "Keys: sales-register, bill-type-register, item-sales, category-sales, gst-b2b, gst-b2c, gst-hsn-summary, gst-credit-notes, einvoice-status, purchase-register, input-gst, stock-summary, stock-movement, transfers, adjustments, receivables-ageing, payables-ageing, collections, payments-made, profit, edits-cancellations.", [
    req("Run report (JSON)", "GET", "/reports/sales-register?from=2026-04-01&to=2027-03-31&page=1&page_size=50"),
    { ...req("Download report (Excel)", "GET", "/reports/gst-hsn-summary?from=2026-04-01&to=2027-03-31&format=xlsx", { desc: "Signed link; 202 + job for more than 5,000 rows." }), _op: "" },
  ]),
  folder("Notifications", "", [
    req("My notifications", "GET", "/notifications?unread_only=true", { tests: [ok(200), save("notification_id", "j.data.length ? j.data[0].id : undefined")] }),
    req("Mark as read", "POST", "/notifications/{{notification_id}}/read", { tests: [ok(204, 404)] }),
    req("Mark all as read", "POST", "/notifications/read-all", { tests: [ok(204)] }),
  ]),
  folder("Tally", "", [
    req("Export to Tally XML", "POST", "/tally/exports", { tests: [ok(202), save("job_id", "j.id")],
      desc: "Incremental: only documents (or new versions) not exported yet. Poll System > Job status for result_url.",
      body: { from: "2026-04-01", to: "2027-03-31", scope: ["masters", "sales", "credit_notes", "receipts", "purchases", "payments"], include_already_exported: false } }),
    req("Import / export history", "GET", "/tally/syncs"),
    req("Upload Tally file (preview)", "POST", "/tally/imports", { tests: [ok(201), save("import_id", "j.import_id")],
      desc: "Sends samples/tally-masters.xml (or pick your Tally export / Excel workbook). Nothing is written yet." + SA,
      form: [{ key: "file", type: "file", src: "samples/tally-masters.xml" }, { key: "cutover_date", value: "2026-04-01", type: "text" },
        { key: "scope", value: "groups,units,items,customers,suppliers,opening_stock,opening_balances", type: "text" }] }),
    req("Commit import", "POST", "/tally/imports/{{import_id}}/commit", { tests: [ok(202), save("job_id", "j.id")],
      desc: "All-or-nothing, as a job. Map unknown godowns and units." + SA, body: { location_map: { "Main Location": "{{godown1_id}}" }, unit_map: {} } }),
  ]),
  folder("Audit", "", [req("Audit log", "GET", "/audit-log?entity=bill&page=1&page_size=50", { desc: SA.trim() })]),
  folder("Cleanup", "Run last: cancels the Cash bill used above.", [
    req("Get bill (for its version)", "GET", "/bills/{{bill_id}}", { tests: [ok(200), etag("bill_version")] }),
    req("Cancel bill", "POST", "/bills/{{bill_id}}/cancel", { headers: ifMatch("bill_version"),
      desc: "Reverses stock and ledger, keeps the number, cancels the IRN if within 24 h. Cancel the Account copy first." + SA,
      body: { reason: "Customer cancelled the order" } }),
  ]),
];

// The Excel download hits the same operation as the JSON run.
folders.find((f) => f.name === "Reports").item[1]._op = "GET /reports/{}";

// ---------------------------------------------------------------- ids as numbers

// "{{x_id}}" placeholders inside JSON bodies become bare {{x_id}} so ids are
// sent as numbers, not strings.
function fixIds(item) {
  if (item.item) return item.item.forEach(fixIds);
  const b = item.request.body;
  if (b && b.mode === "raw") b.raw = b.raw.replace(/"(\{\{(?:[a-z0-9_]*_id|unit_[a-z]+)\}\})"/g, "$1");
}
folders.forEach(fixIds);

// ---------------------------------------------------------------- coverage check

const spec = readFileSync(join(here, "..", "openapi.yaml"), "utf8");
const specOps = [];
let current = null;
for (const ln of spec.split(/\r?\n/)) {
  if (ln.startsWith("components:")) break;
  let m = ln.match(/^  (\/[^:]*):\s*$/);
  if (m) { current = m[1]; continue; }
  m = ln.match(/^    (get|post|put|patch|delete):\s*$/);
  if (m && current) specOps.push(m[1].toUpperCase() + " " + current.replace(/\{[^}]+\}/g, "{}"));
}
const have = new Set();
const collect = (items) => items.forEach((i) => (i.item ? collect(i.item) : i._op && have.add(i._op.replace(/\/\{\}/g, "/{}"))));
collect(folders);
const missing = specOps.filter((op) => !have.has(op));
if (missing.length) {
  console.error("Missing operations:\n  " + missing.join("\n  "));
  process.exit(1);
}
const strip = (items) => items.forEach((i) => (i.item ? strip(i.item) : delete i._op));
strip(folders);

// ---------------------------------------------------------------- write

const collection = {
  info: {
    _postman_id: "6d3c9a52-6f1e-4a8e-9b1f-2c7a3f0b9e11",
    name: "Billing & Inventory API",
    description: [
      "All operations in openapi.yaml, in the order of a working day. Generated by postman/build-collection.mjs.",
      "",
      "1. Import this collection and an environment from postman/.",
      "2. Fill supabase_url, supabase_anon_key, email and password (a Super Admin).",
      "3. Run 'Auth (Supabase) / Sign in'. It stores access_token and a per-run suffix used to keep names unique.",
      "4. Run the folders in order (Collection Runner works). Ids are stored in collection variables as you go.",
      "",
      "File uploads use postman/samples/* (Collection Runner: set the working directory to the postman folder; newman: --working-dir postman).",
      "Money, rates and quantities are decimal strings. Errors are application/problem+json with a stable code.",
    ].join("\n"),
    schema: "https://schema.getpostman.com/json/collection/v2.1.0/collection.json",
  },
  auth: { type: "bearer", bearer: [{ key: "token", value: "{{access_token}}", type: "string" }] },
  variable: [{ key: "baseUrl", value: "{{api_url}}/api/v1" }, { key: "run", value: "" }],
  item: [auth, ...folders],
};
writeFileSync(join(here, "billing-api.postman_collection.json"), JSON.stringify(collection, null, 2) + "\n");
let count = 0;
const countReq = (items) => items.forEach((i) => (i.item ? countReq(i.item) : count++));
countReq(collection.item);
console.log(`wrote ${count} requests; all ${specOps.length} spec operations covered`);
