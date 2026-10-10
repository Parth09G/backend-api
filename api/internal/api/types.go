package api

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------- system

type SignedURL struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Job struct {
	ID         uuid.UUID  `json:"id"`
	Kind       string     `json:"kind"`
	Status     string     `json:"status"`
	Progress   int32      `json:"progress"`
	ResultURL  *string    `json:"result_url"`
	Error      *string    `json:"error"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

type State struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

type SearchItem struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	StockTotal Qty    `json:"stock_total"`
}
type SearchParty struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Mobile string `json:"mobile,omitempty"`
}
type SearchBill struct {
	ID           int64  `json:"id"`
	BillNo       string `json:"bill_no"`
	BillType     string `json:"bill_type"`
	CustomerName string `json:"customer_name"`
	BillDate     Date   `json:"bill_date"`
}
type SearchResult struct {
	Items   []SearchItem  `json:"items"`
	Parties []SearchParty `json:"parties"`
	Bills   []SearchBill  `json:"bills"`
}

// ---------------------------------------------------------------- users

type User struct {
	ID          uuid.UUID  `json:"id"`
	FullName    string     `json:"full_name"`
	Email       string     `json:"email"`
	Mobile      *string    `json:"mobile"`
	Role        string     `json:"role"`
	IsActive    bool       `json:"is_active"`
	LastLoginAt *time.Time `json:"last_login_at"`
	CreatedAt   time.Time  `json:"created_at"`
}

type Me struct {
	User
	Permissions []string `json:"permissions"`
}

type UserInvite struct {
	FullName string  `json:"full_name" validate:"required,min=2,max=100"`
	Email    string  `json:"email" validate:"required,email"`
	Mobile   *string `json:"mobile" validate:"omitempty,max=20"`
	Role     string  `json:"role" validate:"required,oneof=super_admin admin"`
}

type UserPatch struct {
	FullName *string `json:"full_name" validate:"omitempty,min=2,max=100"`
	Mobile   *string `json:"mobile" validate:"omitempty,max=20"`
	Role     *string `json:"role" validate:"omitempty,oneof=super_admin admin"`
}

// ---------------------------------------------------------------- settings

type NotificationChannels struct {
	Email          bool   `json:"email"`
	WhatsApp       bool   `json:"whatsapp"`
	WhatsAppNumber string `json:"whatsapp_number,omitempty"`
}

type Settings struct {
	ShopName             string               `json:"shop_name"`
	Address              string               `json:"address"`
	StateCode            string               `json:"state_code"`
	GSTIN                string               `json:"gstin"`
	Phone                string               `json:"phone"`
	Email                string               `json:"email"`
	LogoURL              *string              `json:"logo_url"`
	BankName             string               `json:"bank_name"`
	BankAccountNo        string               `json:"bank_account_no"`
	BankIFSC             string               `json:"bank_ifsc"`
	UPIID                string               `json:"upi_id"`
	InvoiceTerms         string               `json:"invoice_terms"`
	PricesIncludeGST     bool                 `json:"prices_include_gst"`
	DefaultChargeGST     string               `json:"default_charge_gst"`
	EInvoiceEnabled      bool                 `json:"einvoice_enabled"`
	NumberPrefixes       map[string]string    `json:"number_prefixes"`
	NotificationChannels NotificationChannels `json:"notification_channels"`
}

type SettingsInput struct {
	ShopName             string                `json:"shop_name" validate:"required,max=200"`
	Address              string                `json:"address" validate:"required,max=500"`
	StateCode            string                `json:"state_code" validate:"required,statecode"`
	GSTIN                string                `json:"gstin" validate:"required,gstin"`
	Phone                *string               `json:"phone" validate:"omitempty,max=30"`
	Email                *string               `json:"email" validate:"omitempty,email"`
	LogoURL              *string               `json:"logo_url"` // read-only; set via /settings/logo
	BankName             *string               `json:"bank_name"`
	BankAccountNo        *string               `json:"bank_account_no"`
	BankIFSC             *string               `json:"bank_ifsc"`
	UPIID                *string               `json:"upi_id"`
	InvoiceTerms         *string               `json:"invoice_terms" validate:"omitempty,max=2000"`
	PricesIncludeGST     *bool                 `json:"prices_include_gst"`
	DefaultChargeGST     *string               `json:"default_charge_gst" validate:"omitempty,gstrate"`
	EInvoiceEnabled      *bool                 `json:"einvoice_enabled"`
	NumberPrefixes       map[string]string     `json:"number_prefixes" validate:"omitempty,dive,keys,oneof=cash_bill account_bill credit_note receipt payment_voucher purchase purchase_return transfer adjustment,endkeys,min=1,max=4,alphanum"`
	NotificationChannels *NotificationChannels `json:"notification_channels"`
}

type EInvoiceCredentials struct {
	Provider     string  `json:"provider" validate:"required,max=50"`
	Environment  string  `json:"environment" validate:"required,oneof=sandbox production"`
	Username     string  `json:"username" validate:"required,max=100"`
	Password     string  `json:"password" validate:"required,max=200"`
	ClientID     *string `json:"client_id" validate:"omitempty,max=200"`
	ClientSecret *string `json:"client_secret" validate:"omitempty,max=500"`
}

type TallyMapping struct {
	SalesLedgerCash    string `json:"sales_ledger_cash,omitempty"`
	SalesLedgerAccount string `json:"sales_ledger_account,omitempty"`
	CGSTLedger         string `json:"cgst_ledger,omitempty"`
	SGSTLedger         string `json:"sgst_ledger,omitempty"`
	IGSTLedger         string `json:"igst_ledger,omitempty"`
	LabourLedger       string `json:"labour_ledger,omitempty"`
	TransportLedger    string `json:"transport_ledger,omitempty"`
	RoundOffLedger     string `json:"round_off_ledger,omitempty"`
	CashLedger         string `json:"cash_ledger,omitempty"`
	BankLedger         string `json:"bank_ledger,omitempty"`
	PurchaseLedger     string `json:"purchase_ledger,omitempty"`
	VoucherTypeCash    string `json:"voucher_type_cash,omitempty"`
	VoucherTypeAccount string `json:"voucher_type_account,omitempty"`
}

// ---------------------------------------------------------------- locations, units, categories

type Location struct {
	ID               int64   `json:"id"`
	Name             string  `json:"name"`
	Kind             string  `json:"kind"`
	Address          *string `json:"address"`
	IsDefaultBilling bool    `json:"is_default_billing"`
	IsActive         bool    `json:"is_active"`
}

type LocationInput struct {
	Name             string  `json:"name" validate:"required,max=100"`
	Kind             string  `json:"kind" validate:"required,oneof=shop godown"`
	Address          *string `json:"address" validate:"omitempty,max=500"`
	IsDefaultBilling *bool   `json:"is_default_billing"`
	IsActive         *bool   `json:"is_active"`
}

type Unit struct {
	ID       int64   `json:"id"`
	Name     string  `json:"name"`
	Symbol   string  `json:"symbol"`
	UQCCode  *string `json:"uqc_code"`
	Decimals int32   `json:"decimals"`
}

type UnitInput struct {
	Name     string  `json:"name" validate:"required,max=50"`
	Symbol   string  `json:"symbol" validate:"required,max=20"`
	UQCCode  *string `json:"uqc_code" validate:"omitempty,max=10"`
	Decimals *int32  `json:"decimals" validate:"omitempty,min=0,max=3"`
}

type CategoryAttribute struct {
	ID         int64    `json:"id"`
	Code       string   `json:"code"`
	Label      string   `json:"label"`
	DataType   string   `json:"data_type"`
	Options    []string `json:"options"`
	ValueUnit  *string  `json:"value_unit"`
	IsRequired bool     `json:"is_required"`
	PartOfName bool     `json:"part_of_name"`
	SortOrder  int32    `json:"sort_order"`
}

type CategoryAttributeInput struct {
	Code       string   `json:"code" validate:"required,attrcode,max=40"`
	Label      string   `json:"label" validate:"required,max=60"`
	DataType   string   `json:"data_type" validate:"required,oneof=text number select"`
	Options    []string `json:"options" validate:"omitempty,dive,required,max=60"`
	ValueUnit  *string  `json:"value_unit" validate:"omitempty,max=20"`
	IsRequired *bool    `json:"is_required"`
	PartOfName *bool    `json:"part_of_name"`
	SortOrder  *int32   `json:"sort_order"`
}

type CategoryUnit struct {
	Unit      Unit `json:"unit"`
	IsDefault bool `json:"is_default"`
}

type Category struct {
	ID          int64               `json:"id"`
	Name        string              `json:"name"`
	ParentID    *int64              `json:"parent_id"`
	DefaultHSN  *string             `json:"default_hsn"`
	DefaultGST  *string             `json:"default_gst"`
	Description *string             `json:"description"`
	IsActive    bool                `json:"is_active"`
	TallyGroup  *string             `json:"tally_group"`
	Units       []CategoryUnit      `json:"units"`
	Attributes  []CategoryAttribute `json:"attributes"`
	ItemCount   int64               `json:"item_count"`
}

type CategoryUnitInput struct {
	UnitID    *int64     `json:"unit_id" validate:"omitempty,gt=0"`
	NewUnit   *UnitInput `json:"new_unit"`
	IsDefault bool       `json:"is_default"`
}

type CategoryInput struct {
	Name        string                   `json:"name" validate:"required,max=100"`
	ParentID    *int64                   `json:"parent_id" validate:"omitempty,gt=0"`
	DefaultHSN  *string                  `json:"default_hsn" validate:"omitempty,max=10"`
	DefaultGST  *string                  `json:"default_gst" validate:"omitempty,gstrate"`
	Description *string                  `json:"description" validate:"omitempty,max=500"`
	TallyGroup  *string                  `json:"tally_group" validate:"omitempty,max=100"`
	Units       []CategoryUnitInput      `json:"units" validate:"omitempty,dive"`
	Attributes  []CategoryAttributeInput `json:"attributes" validate:"omitempty,dive"`
}

// ---------------------------------------------------------------- items

type ItemUnit struct {
	UnitID       int64  `json:"unit_id"`
	Symbol       string `json:"symbol"`
	FactorToBase Rate   `json:"factor_to_base"`
}

type StockByLocation struct {
	LocationID   int64  `json:"location_id"`
	LocationName string `json:"location_name"`
	Qty          Qty    `json:"qty"`
}

type Item struct {
	ID               int64             `json:"id"`
	CategoryID       int64             `json:"category_id"`
	CategoryName     string            `json:"category_name"`
	Name             string            `json:"name"`
	Attributes       map[string]any    `json:"attributes"`
	SKU              *string           `json:"sku"`
	HSNCode          string            `json:"hsn_code"`
	GSTRate          string            `json:"gst_rate"`
	BaseUnit         Unit              `json:"base_unit"`
	Units            []ItemUnit        `json:"units"`
	ReorderLevel     Qty               `json:"reorder_level"`
	AvgCost          Rate              `json:"avg_cost"`
	LastPurchaseRate *Rate             `json:"last_purchase_rate"`
	IsActive         bool              `json:"is_active"`
	StockTotal       Qty               `json:"stock_total"`
	Stock            []StockByLocation `json:"stock"`
	StockValue       Money             `json:"stock_value"`
	StockStatus      string            `json:"stock_status"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

type ItemUnitInput struct {
	UnitID       int64  `json:"unit_id" validate:"required,gt=0"`
	FactorToBase string `json:"factor_to_base" validate:"required,rate_pos"`
}

type OpeningStockInput struct {
	LocationID int64   `json:"location_id" validate:"required,gt=0"`
	Qty        string  `json:"qty" validate:"required,qty_pos"`
	UnitCost   *string `json:"unit_cost" validate:"omitempty,rate"`
}

type ItemInput struct {
	CategoryID   int64               `json:"category_id" validate:"required,gt=0"`
	Name         *string             `json:"name" validate:"omitempty,max=200"`
	Attributes   map[string]any      `json:"attributes" validate:"required"`
	SKU          *string             `json:"sku" validate:"omitempty,max=50"`
	HSNCode      string              `json:"hsn_code" validate:"required,max=10"`
	GSTRate      string              `json:"gst_rate" validate:"required,gstrate"`
	BaseUnitID   int64               `json:"base_unit_id" validate:"required,gt=0"`
	Units        []ItemUnitInput     `json:"units" validate:"omitempty,dive"`
	ReorderLevel *string             `json:"reorder_level" validate:"omitempty,qty_nn"`
	OpeningStock []OpeningStockInput `json:"opening_stock" validate:"omitempty,dive"`
}

type StockMovement struct {
	ID           int64     `json:"id"`
	Date         time.Time `json:"date"`
	LocationID   int64     `json:"location_id"`
	LocationName string    `json:"location_name"`
	Kind         string    `json:"kind"`
	QtyIn        Qty       `json:"qty_in"`
	QtyOut       Qty       `json:"qty_out"`
	Balance      Qty       `json:"balance"`
	UnitCost     Rate      `json:"unit_cost"`
	RefType      string    `json:"ref_type,omitempty"`
	RefID        *int64    `json:"ref_id,omitempty"`
	RefNo        string    `json:"ref_no,omitempty"`
	UserName     string    `json:"user_name,omitempty"`
}

type LastRate struct {
	Rate   Money  `json:"rate"`
	Unit   string `json:"unit"`
	BillNo string `json:"bill_no"`
	Date   Date   `json:"date"`
}

type RecentRate struct {
	Rate     Money  `json:"rate"`
	Unit     string `json:"unit"`
	Customer string `json:"customer"`
	Date     Date   `json:"date"`
}

type PriceHints struct {
	LastRateToCustomer *LastRate    `json:"last_rate_to_customer"`
	RecentRates        []RecentRate `json:"recent_rates"`
	LastPurchaseRate   *Rate        `json:"last_purchase_rate"`
	AvgCost            Rate         `json:"avg_cost"`
	Unit               string       `json:"unit"`
}

// ---------------------------------------------------------------- parties

type PartySite struct {
	ID      int64  `json:"id"`
	Label   string `json:"label"`
	Address string `json:"address"`
}

type Party struct {
	ID             int64       `json:"id"`
	Kind           string      `json:"kind"`
	Name           string      `json:"name"`
	Mobile         *string     `json:"mobile"`
	AltPhone       *string     `json:"alt_phone"`
	GSTIN          *string     `json:"gstin"`
	StateCode      *string     `json:"state_code"`
	BillingAddress *string     `json:"billing_address"`
	Pincode        *string     `json:"pincode"`
	CreditLimit    *Money      `json:"credit_limit"`
	OpeningBalance Money       `json:"opening_balance"`
	Balance        Money       `json:"balance"`
	Sites          []PartySite `json:"sites"`
	Notes          *string     `json:"notes"`
	IsActive       bool        `json:"is_active"`
}

type SiteInput struct {
	Label   string `json:"label" validate:"required,max=100"`
	Address string `json:"address" validate:"required,max=500"`
}

type PartyInput struct {
	Kind           string      `json:"kind" validate:"required,oneof=customer supplier both"`
	Name           string      `json:"name" validate:"required,min=2,max=200"`
	Mobile         *string     `json:"mobile" validate:"omitempty,max=20"`
	AltPhone       *string     `json:"alt_phone" validate:"omitempty,max=20"`
	GSTIN          *string     `json:"gstin" validate:"omitempty,gstin"`
	StateCode      *string     `json:"state_code" validate:"omitempty,statecode"`
	BillingAddress *string     `json:"billing_address" validate:"omitempty,max=500"`
	Pincode        *string     `json:"pincode" validate:"omitempty,len=6,numeric"`
	CreditLimit    *string     `json:"credit_limit" validate:"omitempty,money_nn"`
	OpeningBalance *string     `json:"opening_balance" validate:"omitempty,money"`
	OpeningDate    *string     `json:"opening_date" validate:"omitempty,date"`
	Notes          *string     `json:"notes" validate:"omitempty,max=1000"`
	Sites          []SiteInput `json:"sites" validate:"omitempty,dive"`
}

type LedgerEntry struct {
	ID        int64  `json:"id"`
	Date      Date   `json:"date"`
	RefType   string `json:"ref_type"`
	RefID     int64  `json:"ref_id"`
	RefNo     string `json:"ref_no"`
	Narration string `json:"narration"`
	Debit     Money  `json:"debit"`
	Credit    Money  `json:"credit"`
	Balance   Money  `json:"balance"`
}

type Ledger struct {
	Party   Party         `json:"party"`
	Opening Money         `json:"opening"`
	Entries []LedgerEntry `json:"entries"`
	Closing Money         `json:"closing"`
}

// ---------------------------------------------------------------- bills

type PaymentSplitInput struct {
	Mode       string  `json:"mode" validate:"required,oneof=cash upi cheque bank_transfer"`
	Amount     string  `json:"amount" validate:"required,money_pos"`
	Reference  *string `json:"reference" validate:"omitempty,max=100"`
	ChequeNo   *string `json:"cheque_no" validate:"required_if=Mode cheque,omitempty,max=20"`
	ChequeDate *string `json:"cheque_date" validate:"omitempty,date"`
	BankName   *string `json:"bank_name" validate:"omitempty,max=100"`
}

type BillLineInput struct {
	ItemID      int64   `json:"item_id" validate:"required,gt=0"`
	LocationID  *int64  `json:"location_id" validate:"omitempty,gt=0"`
	Qty         string  `json:"qty" validate:"required,qty_pos"`
	UnitID      int64   `json:"unit_id" validate:"required,gt=0"`
	Rate        string  `json:"rate" validate:"required,money_nn"`
	DiscountPct *string `json:"discount_pct" validate:"omitempty,percent"`
	DiscountAmt *string `json:"discount_amt" validate:"omitempty,money_nn"`
	Description *string `json:"description" validate:"omitempty,max=300"`
}

type BillChargeInput struct {
	Kind        string  `json:"kind" validate:"required,oneof=labour transport other"`
	Description *string `json:"description" validate:"omitempty,max=200"`
	Amount      string  `json:"amount" validate:"required,money_nn"`
	GSTRate     *string `json:"gst_rate" validate:"omitempty,gstrate"`
	SACCode     *string `json:"sac_code" validate:"omitempty,max=10"`
}

type CustomerInput struct {
	PartyID        *int64  `json:"party_id" validate:"omitempty,gt=0"`
	WalkInName     *string `json:"walk_in_name" validate:"omitempty,max=200"`
	GSTIN          *string `json:"gstin" validate:"omitempty,gstin"`
	BillingAddress *string `json:"billing_address" validate:"omitempty,max=500"`
	Mobile         *string `json:"mobile" validate:"omitempty,max=20"`
	SiteAddress    *string `json:"site_address" validate:"omitempty,max=500"`
}

type BillInput struct {
	BillType            string              `json:"bill_type" validate:"required,oneof=cash account"`
	Status              string              `json:"status" validate:"required,oneof=draft active"`
	BillDate            *string             `json:"bill_date" validate:"omitempty,date"`
	Customer            CustomerInput       `json:"customer" validate:"required"`
	PlaceOfSupply       string              `json:"place_of_supply" validate:"required,statecode"`
	VehicleNo           *string             `json:"vehicle_no" validate:"omitempty,max=20"`
	Lines               []BillLineInput     `json:"lines" validate:"required,min=1,max=200,dive"`
	Charges             []BillChargeInput   `json:"charges" validate:"omitempty,max=20,dive"`
	BillDiscount        *string             `json:"bill_discount" validate:"omitempty,money_nn"`
	Payments            []PaymentSplitInput `json:"payments" validate:"omitempty,max=10,dive"`
	Notes               *string             `json:"notes" validate:"omitempty,max=1000"`
	OverrideCreditLimit bool                `json:"override_credit_limit"`
	ExpectedTotal       *string             `json:"expected_total" validate:"omitempty,money"`
}

type BillEditInput struct {
	BillInput
	Reason string `json:"reason" validate:"omitempty,min=3,max=500"`
}

type TaxSplit struct {
	GSTRate string `json:"gst_rate"`
	Taxable Money  `json:"taxable"`
	CGST    Money  `json:"cgst"`
	SGST    Money  `json:"sgst"`
	IGST    Money  `json:"igst"`
}

type BillTotals struct {
	ItemsTaxable   Money      `json:"items_taxable"`
	ChargesTaxable Money      `json:"charges_taxable"`
	BillDiscount   Money      `json:"bill_discount"`
	TaxableTotal   Money      `json:"taxable_total"`
	CGST           Money      `json:"cgst"`
	SGST           Money      `json:"sgst"`
	IGST           Money      `json:"igst"`
	TaxByRate      []TaxSplit `json:"tax_by_rate"`
	RoundOff       Money      `json:"round_off"`
	GrandTotal     Money      `json:"grand_total"`
	AmountInWords  string     `json:"amount_in_words"`
	AmountPaid     Money      `json:"amount_paid"`
	BalanceDue     Money      `json:"balance_due"`
}

type BillLine struct {
	ID           int64   `json:"id,omitempty"`
	LineNo       int32   `json:"line_no"`
	ItemID       int64   `json:"item_id"`
	LocationID   *int64  `json:"location_id"`
	LocationName *string `json:"location_name"`
	Description  string  `json:"description"`
	HSNCode      string  `json:"hsn_code"`
	Qty          Qty     `json:"qty"`
	UnitID       int64   `json:"unit_id"`
	Unit         string  `json:"unit"`
	BaseQty      Qty     `json:"base_qty"`
	Rate         Money   `json:"rate"`
	DiscountPct  string  `json:"discount_pct"`
	DiscountAmt  Money   `json:"discount_amt"`
	TaxableValue Money   `json:"taxable_value"`
	GSTRate      string  `json:"gst_rate"`
	CGST         Money   `json:"cgst"`
	SGST         Money   `json:"sgst"`
	IGST         Money   `json:"igst"`
	LineTotal    Money   `json:"line_total"`
	UnitCost     *Rate   `json:"unit_cost"`
}

type BillCharge struct {
	ID          int64   `json:"id,omitempty"`
	Kind        string  `json:"kind"`
	Description string  `json:"description"`
	SACCode     *string `json:"sac_code"`
	Amount      Money   `json:"amount"`
	GSTRate     string  `json:"gst_rate"`
	CGST        Money   `json:"cgst"`
	SGST        Money   `json:"sgst"`
	IGST        Money   `json:"igst"`
}

type EInvoice struct {
	Status         string     `json:"status"`
	IRN            *string    `json:"irn"`
	AckNo          *string    `json:"ack_no"`
	AckDate        *time.Time `json:"ack_date"`
	SignedQR       *string    `json:"signed_qr"`
	CancelDeadline *time.Time `json:"cancel_deadline"`
	EWBNo          *string    `json:"ewb_no"`
	EWBValidUntil  *time.Time `json:"ewb_valid_until"`
	LastError      *string    `json:"last_error"`
}

type BillRef struct {
	ID     int64  `json:"id"`
	BillNo string `json:"bill_no"`
}

type BillCustomer struct {
	PartyID     *int64  `json:"party_id"`
	Name        string  `json:"name"`
	GSTIN       *string `json:"gstin"`
	Address     *string `json:"address"`
	Mobile      *string `json:"mobile"`
	SiteAddress *string `json:"site_address"`
}

type BillPayment struct {
	PaymentID int64  `json:"payment_id"`
	DocNo     string `json:"doc_no"`
	Mode      string `json:"mode"`
	Amount    Money  `json:"amount"`
	Date      Date   `json:"date"`
}

type Bill struct {
	ID             int64         `json:"id"`
	BillType       string        `json:"bill_type"`
	BillNo         *string       `json:"bill_no"`
	FY             *string       `json:"fy"`
	BillDate       Date          `json:"bill_date"`
	Status         string        `json:"status"`
	Version        int32         `json:"version"`
	SourceBill     *BillRef      `json:"source_bill"`
	AccountCopy    *BillRef      `json:"account_copy"`
	Customer       BillCustomer  `json:"customer"`
	PlaceOfSupply  string        `json:"place_of_supply"`
	IsInterstate   bool          `json:"is_interstate"`
	VehicleNo      *string       `json:"vehicle_no"`
	AffectsStock   bool          `json:"affects_stock"`
	PostsToLedger  bool          `json:"posts_to_ledger"`
	Lines          []BillLine    `json:"lines"`
	Charges        []BillCharge  `json:"charges"`
	Totals         BillTotals    `json:"totals"`
	Payments       []BillPayment `json:"payments"`
	CostTotal      Money         `json:"cost_total"`
	GrossProfit    Money         `json:"gross_profit"`
	EInvoice       EInvoice      `json:"einvoice"`
	Notes          *string       `json:"notes"`
	AllowedActions []string      `json:"allowed_actions"`
	CancelReason   *string       `json:"cancel_reason"`
	CreatedByName  string        `json:"created_by_name"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
}

type BillSummary struct {
	ID             int64   `json:"id"`
	BillType       string  `json:"bill_type"`
	BillNo         *string `json:"bill_no"`
	BillDate       Date    `json:"bill_date"`
	Status         string  `json:"status"`
	CustomerName   string  `json:"customer_name"`
	PartyID        *int64  `json:"party_id"`
	GrandTotal     Money   `json:"grand_total"`
	BalanceDue     Money   `json:"balance_due"`
	PaymentStatus  string  `json:"payment_status"`
	EInvoiceStatus string  `json:"einvoice_status"`
	IsAccountCopy  bool    `json:"is_account_copy"`
	SourceBillID   *int64  `json:"source_bill_id"`
	AccountCopyID  *int64  `json:"account_copy_id"`
	Version        int32   `json:"version"`
	CreatedByName  string  `json:"created_by_name"`
}

type BillRevision struct {
	Version      int32           `json:"version"`
	Reason       string          `json:"reason"`
	EditedByName string          `json:"edited_by_name"`
	EditedAt     time.Time       `json:"edited_at"`
	Snapshot     json.RawMessage `json:"snapshot"`
}

type CopyToAccountInput struct {
	BillDate *string `json:"bill_date" validate:"omitempty,date"`
	Status   *string `json:"status" validate:"omitempty,oneof=draft active"`
	Notes    *string `json:"notes" validate:"omitempty,max=1000"`
}

type ShortLineElsewhere struct {
	LocationID   int64  `json:"location_id"`
	LocationName string `json:"location_name"`
	Qty          Qty    `json:"qty"`
}

type ShortLine struct {
	LineNo             int                  `json:"line_no"`
	ItemID             int64                `json:"item_id"`
	ItemName           string               `json:"item_name"`
	LocationID         int64                `json:"location_id"`
	LocationName       string               `json:"location_name"`
	Required           Qty                  `json:"required"`
	Available          Qty                  `json:"available"`
	Unit               string               `json:"unit"`
	AvailableElsewhere []ShortLineElsewhere `json:"available_elsewhere"`
}

type CreditWarning struct {
	Title       string `json:"title"`
	Status      int    `json:"status"`
	Code        string `json:"code"`
	Detail      string `json:"detail,omitempty"`
	CreditLimit Money  `json:"credit_limit"`
	Outstanding Money  `json:"outstanding"`
	ThisBill    Money  `json:"this_bill"`
}

type BillCalculation struct {
	Lines         []BillLine     `json:"lines"`
	Charges       []BillCharge   `json:"charges"`
	Totals        BillTotals     `json:"totals"`
	StockWarnings []ShortLine    `json:"stock_warnings"`
	CreditWarning *CreditWarning `json:"credit_warning"`
}

type ReasonInput struct {
	Reason string `json:"reason" validate:"required,min=3,max=500"`
}

type EWayBillInput struct {
	VehicleNo     string  `json:"vehicle_no" validate:"required,max=20"`
	TransportMode *string `json:"transport_mode" validate:"omitempty,oneof=road rail air ship"`
	DistanceKm    int     `json:"distance_km" validate:"gte=0,lte=4000"`
	TransporterID *string `json:"transporter_id" validate:"omitempty,max=20"`
}

type ShareResult struct {
	WhatsAppURL string `json:"whatsapp_url"`
	PDFURL      string `json:"pdf_url"`
}

// ---------------------------------------------------------------- payments

type AllocationInput struct {
	RefType string `json:"ref_type" validate:"required,oneof=cash_bill account_bill purchase"`
	RefID   int64  `json:"ref_id" validate:"required,gt=0"`
	Amount  string `json:"amount" validate:"required,money_pos"`
}

type PaymentInput struct {
	Direction    string            `json:"direction" validate:"required,oneof=received paid"`
	PartyID      int64             `json:"party_id" validate:"required,gt=0"`
	PayDate      *string           `json:"pay_date" validate:"omitempty,date"`
	Mode         string            `json:"mode" validate:"required,oneof=cash upi cheque bank_transfer"`
	Amount       string            `json:"amount" validate:"required,money_pos"`
	Reference    *string           `json:"reference" validate:"omitempty,max=100"`
	ChequeNo     *string           `json:"cheque_no" validate:"required_if=Mode cheque,omitempty,max=20"`
	ChequeDate   *string           `json:"cheque_date" validate:"omitempty,date"`
	BankName     *string           `json:"bank_name" validate:"omitempty,max=100"`
	Notes        *string           `json:"notes" validate:"omitempty,max=500"`
	Allocations  []AllocationInput `json:"allocations" validate:"omitempty,max=100,dive"`
	AutoAllocate bool              `json:"auto_allocate"`
}

type ChequeStatusInput struct {
	Status string  `json:"status" validate:"required,oneof=cleared bounced"`
	Date   *string `json:"date" validate:"omitempty,date"`
	Notes  *string `json:"notes" validate:"omitempty,max=500"`
}

type PaymentAllocation struct {
	RefType string `json:"ref_type"`
	RefID   int64  `json:"ref_id"`
	RefNo   string `json:"ref_no"`
	Amount  Money  `json:"amount"`
}

type Payment struct {
	ID           int64               `json:"id"`
	DocNo        string              `json:"doc_no"`
	Direction    string              `json:"direction"`
	PartyID      *int64              `json:"party_id"`
	PartyName    string              `json:"party_name"`
	PayDate      Date                `json:"pay_date"`
	Mode         string              `json:"mode"`
	Amount       Money               `json:"amount"`
	Reference    *string             `json:"reference"`
	ChequeNo     *string             `json:"cheque_no"`
	ChequeDate   *Date               `json:"cheque_date"`
	BankName     *string             `json:"bank_name"`
	ChequeStatus *string             `json:"cheque_status"`
	Status       string              `json:"status"`
	Allocations  []PaymentAllocation `json:"allocations"`
	Unallocated  Money               `json:"unallocated"`
}

// ---------------------------------------------------------------- returns

type ReturnLineInput struct {
	BillLineID int64  `json:"bill_line_id" validate:"required,gt=0"`
	Qty        string `json:"qty" validate:"required,qty_pos"`
	UnitID     int64  `json:"unit_id" validate:"required,gt=0"`
	LocationID *int64 `json:"location_id" validate:"omitempty,gt=0"`
	Restock    *bool  `json:"restock"`
}

type ReturnInput struct {
	BillID     int64              `json:"bill_id" validate:"required,gt=0"`
	CNDate     *string            `json:"cn_date" validate:"omitempty,date"`
	Reason     string             `json:"reason" validate:"required,min=3,max=500"`
	Settlement string             `json:"settlement" validate:"required,oneof=refund adjust_ledger"`
	Refund     *PaymentSplitInput `json:"refund"`
	Lines      []ReturnLineInput  `json:"lines" validate:"required,min=1,max=200,dive"`
}

type CreditNoteLine struct {
	BillLineID   int64  `json:"bill_line_id"`
	ItemID       int64  `json:"item_id"`
	Description  string `json:"description"`
	Qty          Qty    `json:"qty"`
	Unit         string `json:"unit"`
	LocationID   int64  `json:"location_id"`
	Rate         Money  `json:"rate"`
	TaxableValue Money  `json:"taxable_value"`
	Restock      bool   `json:"restock"`
}

type CreditNote struct {
	ID             int64            `json:"id"`
	CNNo           *string          `json:"cn_no"`
	CNDate         Date             `json:"cn_date"`
	BillID         int64            `json:"bill_id"`
	BillNo         string           `json:"bill_no"`
	PartyName      string           `json:"party_name"`
	Status         string           `json:"status"`
	Reason         string           `json:"reason"`
	Settlement     string           `json:"settlement"`
	Lines          []CreditNoteLine `json:"lines"`
	Totals         BillTotals       `json:"totals"`
	EInvoice       EInvoice         `json:"einvoice"`
	CreatedByName  string           `json:"created_by_name"`
	ApprovedByName *string          `json:"approved_by_name"`
	ApprovedAt     *time.Time       `json:"approved_at"`
}

// ---------------------------------------------------------------- purchases

type PurchaseLineInput struct {
	ItemID      int64   `json:"item_id" validate:"required,gt=0"`
	LocationID  *int64  `json:"location_id" validate:"omitempty,gt=0"`
	Qty         string  `json:"qty" validate:"required,qty_pos"`
	UnitID      int64   `json:"unit_id" validate:"required,gt=0"`
	Rate        string  `json:"rate" validate:"required,rate"`
	DiscountAmt *string `json:"discount_amt" validate:"omitempty,money_nn"`
	GSTRate     *string `json:"gst_rate" validate:"omitempty,gstrate"`
}

type PurchaseInput struct {
	SupplierID        int64               `json:"supplier_id" validate:"required,gt=0"`
	SupplierInvoiceNo string              `json:"supplier_invoice_no" validate:"required,max=50"`
	SupplierInvoiceDt string              `json:"supplier_invoice_dt" validate:"required,date"`
	ReceivedDate      *string             `json:"received_date" validate:"omitempty,date"`
	LocationID        int64               `json:"location_id" validate:"required,gt=0"`
	IsInterstate      *bool               `json:"is_interstate"`
	Freight           *string             `json:"freight" validate:"omitempty,money_nn"`
	OtherCharges      *string             `json:"other_charges" validate:"omitempty,money_nn"`
	Lines             []PurchaseLineInput `json:"lines" validate:"required,min=1,max=200,dive"`
	Payment           *PaymentSplitInput  `json:"payment"`
	Notes             *string             `json:"notes" validate:"omitempty,max=1000"`
}

type PurchaseLine struct {
	ID             int64  `json:"id"`
	ItemID         int64  `json:"item_id"`
	ItemName       string `json:"item_name"`
	LocationID     int64  `json:"location_id"`
	Qty            Qty    `json:"qty"`
	Unit           string `json:"unit"`
	BaseQty        Qty    `json:"base_qty"`
	Rate           Rate   `json:"rate"`
	TaxableValue   Money  `json:"taxable_value"`
	GSTRate        string `json:"gst_rate"`
	LandedUnitCost Rate   `json:"landed_unit_cost"`
}

type Purchase struct {
	ID                int64          `json:"id"`
	DocNo             string         `json:"doc_no"`
	SupplierID        int64          `json:"supplier_id"`
	SupplierName      string         `json:"supplier_name"`
	SupplierInvoiceNo string         `json:"supplier_invoice_no"`
	SupplierInvoiceDt Date           `json:"supplier_invoice_dt"`
	ReceivedDate      Date           `json:"received_date"`
	LocationID        int64          `json:"location_id"`
	LocationName      string         `json:"location_name"`
	Lines             []PurchaseLine `json:"lines"`
	Totals            BillTotals     `json:"totals"`
	Status            string         `json:"status"`
	BalancePayable    Money          `json:"balance_payable"`
}

type PurchaseReturnLineInput struct {
	PurchaseLineID int64  `json:"purchase_line_id" validate:"required,gt=0"`
	Qty            string `json:"qty" validate:"required,qty_pos"`
	UnitID         int64  `json:"unit_id" validate:"required,gt=0"`
}

type PurchaseReturnInput struct {
	PurchaseID int64                     `json:"purchase_id" validate:"required,gt=0"`
	ReturnDate *string                   `json:"return_date" validate:"omitempty,date"`
	Reason     string                    `json:"reason" validate:"required,min=3,max=500"`
	Lines      []PurchaseReturnLineInput `json:"lines" validate:"required,min=1,max=200,dive"`
}

type DocCreated struct {
	ID    int64  `json:"id"`
	DocNo string `json:"doc_no"`
}

// ---------------------------------------------------------------- stock

type QtyLineInput struct {
	ItemID int64  `json:"item_id" validate:"required,gt=0"`
	Qty    string `json:"qty" validate:"required,qty_pos"`
	UnitID int64  `json:"unit_id" validate:"required,gt=0"`
}

type TransferInput struct {
	FromLocationID int64          `json:"from_location_id" validate:"required,gt=0"`
	ToLocationID   int64          `json:"to_location_id" validate:"required,gt=0,nefield=FromLocationID"`
	TransferDate   *string        `json:"transfer_date" validate:"omitempty,date"`
	VehicleNo      *string        `json:"vehicle_no" validate:"omitempty,max=20"`
	Notes          *string        `json:"notes" validate:"omitempty,max=500"`
	Lines          []QtyLineInput `json:"lines" validate:"required,min=1,max=200,dive"`
}

type TransferLine struct {
	ItemID   int64  `json:"item_id"`
	ItemName string `json:"item_name"`
	BaseQty  Qty    `json:"base_qty"`
	Unit     string `json:"unit"`
}

type Transfer struct {
	ID             int64          `json:"id"`
	DocNo          string         `json:"doc_no"`
	TransferDate   Date           `json:"transfer_date"`
	FromLocationID int64          `json:"from_location_id"`
	ToLocationID   int64          `json:"to_location_id"`
	Lines          []TransferLine `json:"lines"`
	CreatedByName  string         `json:"created_by_name"`
}

type AdjustmentLineInput struct {
	ItemID    int64  `json:"item_id" validate:"required,gt=0"`
	QtyChange string `json:"qty_change" validate:"required,qty_nz"`
}

type AdjustmentInput struct {
	LocationID int64                 `json:"location_id" validate:"required,gt=0"`
	AdjDate    *string               `json:"adj_date" validate:"omitempty,date"`
	Reason     string                `json:"reason" validate:"required,oneof=damage breakage wastage count_correction sample other"`
	Notes      *string               `json:"notes" validate:"omitempty,max=500"`
	Lines      []AdjustmentLineInput `json:"lines" validate:"required,min=1,max=500,dive"`
}

type AdjustmentLine struct {
	ItemID    int64  `json:"item_id"`
	ItemName  string `json:"item_name"`
	QtyChange Qty    `json:"qty_change"`
}

type Adjustment struct {
	ID            int64            `json:"id"`
	DocNo         string           `json:"doc_no"`
	AdjDate       Date             `json:"adj_date"`
	LocationID    int64            `json:"location_id"`
	Reason        string           `json:"reason"`
	Notes         *string          `json:"notes"`
	Lines         []AdjustmentLine `json:"lines"`
	CreatedByName string           `json:"created_by_name"`
}

type StockCountLineInput struct {
	ItemID     int64  `json:"item_id" validate:"required,gt=0"`
	CountedQty string `json:"counted_qty" validate:"required,qty_nn"`
}

type StockCountInput struct {
	LocationID int64                 `json:"location_id" validate:"required,gt=0"`
	Lines      []StockCountLineInput `json:"lines" validate:"required,min=1,max=5000,dive"`
}

type StockCountLine struct {
	ItemID          int64  `json:"item_id"`
	ItemName        string `json:"item_name"`
	SystemQty       Qty    `json:"system_qty"`
	CountedQty      Qty    `json:"counted_qty"`
	Difference      Qty    `json:"difference"`
	ValueDifference Money  `json:"value_difference"`
}

type StockCountPreview struct {
	LocationID int64            `json:"location_id"`
	Lines      []StockCountLine `json:"lines"`
}

// ---------------------------------------------------------------- dashboard

type Period struct {
	From Date `json:"from"`
	To   Date `json:"to"`
}

type LocationValue struct {
	LocationID int64  `json:"location_id"`
	Name       string `json:"name"`
	Value      Money  `json:"value"`
}

type StockSummary struct {
	Items      int64           `json:"items"`
	Value      Money           `json:"value"`
	LowCount   int64           `json:"low_count"`
	OutCount   int64           `json:"out_count"`
	ByLocation []LocationValue `json:"by_location"`
}

type SalesSummary struct {
	Today        Money `json:"today"`
	Period       Money `json:"period"`
	FY           Money `json:"fy"`
	CashBills    Money `json:"cash_bills"`
	AccountBills Money `json:"account_bills"`
	BillCount    int64 `json:"bill_count"`
}

type ModeAmount struct {
	Mode   string `json:"mode"`
	Amount Money  `json:"amount"`
}

type DashboardSummary struct {
	Period            Period       `json:"period"`
	Stock             StockSummary `json:"stock"`
	Sales             SalesSummary `json:"sales"`
	GrossProfit       Money        `json:"gross_profit"`
	GrossMarginPct    string       `json:"gross_margin_pct"`
	Receivables       Money        `json:"receivables"`
	Payables          Money        `json:"payables"`
	ChequesPending    Money        `json:"cheques_pending"`
	CollectionsByMode []ModeAmount `json:"collections_by_mode"`
}

type TrendPoint struct {
	Bucket string `json:"bucket"`
	Sales  Money  `json:"sales"`
	Profit Money  `json:"profit"`
	Bills  int64  `json:"bills"`
}

type CategoryBreakdown struct {
	CategoryID    int64  `json:"category_id"`
	Name          string `json:"name"`
	StockQtyItems int64  `json:"stock_qty_items"`
	StockValue    Money  `json:"stock_value"`
	Sales         Money  `json:"sales"`
	Profit        Money  `json:"profit"`
}

type TopItem struct {
	ItemID  int64  `json:"item_id"`
	Name    string `json:"name"`
	QtySold Qty    `json:"qty_sold"`
	Unit    string `json:"unit"`
	Sales   Money  `json:"sales"`
}

type PendingWork struct {
	EInvoiceFailed    int64 `json:"einvoice_failed"`
	EInvoicePending   int64 `json:"einvoice_pending"`
	ReturnsPending    int64 `json:"returns_pending"`
	BlockedBillsToday int64 `json:"blocked_bills_today"`
	TallyUnexported   int64 `json:"tally_unexported"`
	ChequesDue        int64 `json:"cheques_due"`
}

// ---------------------------------------------------------------- reports, notifications, tally, audit

type ReportColumn struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Type  string `json:"type"` // text | money | qty | date | percent
}

type Report struct {
	Columns []ReportColumn   `json:"columns"`
	Rows    []map[string]any `json:"rows"`
	Totals  map[string]any   `json:"totals"`
	Meta    struct {
		Page     int `json:"page"`
		PageSize int `json:"page_size"`
		Total    int `json:"total"`
	} `json:"meta"`
}

type Notification struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Body      *string   `json:"body"`
	RefType   *string   `json:"ref_type"`
	RefID     *int64    `json:"ref_id"`
	ItemID    *int64    `json:"item_id"`
	IsRead    bool      `json:"is_read"`
	CreatedAt time.Time `json:"created_at"`
}

type ImportCount struct {
	New      int `json:"new"`
	Existing int `json:"existing"`
	Errors   int `json:"errors"`
}

type ImportIssue struct {
	Entity     string `json:"entity"`
	Name       string `json:"name"`
	Problem    string `json:"problem"`
	Suggestion string `json:"suggestion,omitempty"`
}

type TallyImportPreview struct {
	ImportID    int64                  `json:"import_id"`
	Source      string                 `json:"source"`
	CutoverDate Date                   `json:"cutover_date"`
	Counts      map[string]ImportCount `json:"counts"`
	Issues      []ImportIssue          `json:"issues"`
	ExpiresAt   time.Time              `json:"expires_at"`
}

type TallyCommitInput struct {
	LocationMap map[string]int64 `json:"location_map"`
	UnitMap     map[string]int64 `json:"unit_map"`
}

type TallyExportInput struct {
	From                   string   `json:"from" validate:"required,date"`
	To                     string   `json:"to" validate:"required,date"`
	Scope                  []string `json:"scope" validate:"required,min=1,dive,oneof=masters sales credit_notes receipts purchases payments"`
	IncludeAlreadyExported bool     `json:"include_already_exported"`
}

type TallySync struct {
	ID            int64     `json:"id"`
	Direction     string    `json:"direction"`
	Scope         string    `json:"scope"`
	PeriodFrom    *Date     `json:"period_from"`
	PeriodTo      *Date     `json:"period_to"`
	RecordCount   int32     `json:"record_count"`
	Status        string    `json:"status"`
	CreatedByName string    `json:"created_by_name"`
	CreatedAt     time.Time `json:"created_at"`
}

type AuditEntry struct {
	ID        int64           `json:"id"`
	UserName  *string         `json:"user_name"`
	Action    string          `json:"action"`
	Entity    string          `json:"entity"`
	EntityID  *int64          `json:"entity_id"`
	Before    json.RawMessage `json:"before"`
	After     json.RawMessage `json:"after"`
	IPAddress *string         `json:"ip_address"`
	CreatedAt time.Time       `json:"created_at"`
}
