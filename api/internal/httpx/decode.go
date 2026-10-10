package httpx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/shopspring/decimal"
)

const maxBody = 2 << 20 // 2 MiB for JSON bodies

var (
	reMoney   = regexp.MustCompile(`^-?\d{1,12}(\.\d{1,2})?$`)
	reRate    = regexp.MustCompile(`^\d{1,10}(\.\d{1,4})?$`)
	reQty     = regexp.MustCompile(`^-?\d{1,11}(\.\d{1,3})?$`)
	rePercent = regexp.MustCompile(`^\d{1,3}(\.\d{1,2})?$`)
	reState   = regexp.MustCompile(`^\d{2}$`)
	reGSTIN   = regexp.MustCompile(`^[0-9]{2}[A-Z]{5}[0-9]{4}[A-Z][0-9A-Z]Z[0-9A-Z]$`)
	reAttr    = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// GSTRates are the allowed GST rates (schema item.gst_rate CHECK).
var GSTRates = map[string]bool{"0": true, "0.25": true, "3": true, "5": true, "12": true, "18": true, "28": true, "40": true}

var validate = newValidator()

func newValidator() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	v.RegisterTagNameFunc(func(f reflect.StructField) string {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			return ""
		}
		return name
	})
	re := func(rx *regexp.Regexp, check func(decimal.Decimal) bool) validator.Func {
		return func(fl validator.FieldLevel) bool {
			s := fl.Field().String()
			if !rx.MatchString(s) {
				return false
			}
			if check == nil {
				return true
			}
			d, err := decimal.NewFromString(s)
			return err == nil && check(d)
		}
	}
	pos := func(d decimal.Decimal) bool { return d.IsPositive() }
	nonNeg := func(d decimal.Decimal) bool { return !d.IsNegative() }
	_ = v.RegisterValidation("money", re(reMoney, nil))
	_ = v.RegisterValidation("money_pos", re(reMoney, pos))
	_ = v.RegisterValidation("money_nn", re(reMoney, nonNeg))
	_ = v.RegisterValidation("rate", re(reRate, nil))
	_ = v.RegisterValidation("rate_pos", re(reRate, pos))
	_ = v.RegisterValidation("qty", re(reQty, nil))
	_ = v.RegisterValidation("qty_pos", re(reQty, pos))
	_ = v.RegisterValidation("qty_nn", re(reQty, nonNeg))
	_ = v.RegisterValidation("qty_nz", re(reQty, func(d decimal.Decimal) bool { return !d.IsZero() }))
	_ = v.RegisterValidation("percent", re(rePercent, func(d decimal.Decimal) bool { return d.LessThanOrEqual(decimal.NewFromInt(100)) }))
	_ = v.RegisterValidation("statecode", re(reState, nil))
	_ = v.RegisterValidation("gstin", re(reGSTIN, nil))
	_ = v.RegisterValidation("attrcode", re(reAttr, nil))
	_ = v.RegisterValidation("gstrate", func(fl validator.FieldLevel) bool { return GSTRates[fl.Field().String()] })
	_ = v.RegisterValidation("date", func(fl validator.FieldLevel) bool {
		_, err := time.Parse(time.DateOnly, fl.Field().String())
		return err == nil
	})
	return v
}

var messages = map[string]string{
	"required":         "is required",
	"min":              "is too short or too small",
	"max":              "is too long or too large",
	"oneof":            "must be one of: %s",
	"email":            "must be a valid email address",
	"money":            "must be an amount like \"1250.50\"",
	"money_pos":        "must be an amount greater than zero",
	"money_nn":         "must be zero or a positive amount",
	"rate":             "must be a rate like \"62.5000\"",
	"rate_pos":         "must be a rate greater than zero",
	"qty":              "must be a quantity like \"1.500\"",
	"qty_pos":          "must be a quantity greater than zero",
	"qty_nn":           "must be zero or a positive quantity",
	"qty_nz":           "must be a non-zero quantity",
	"percent":          "must be a percentage between 0 and 100",
	"statecode":        "must be a 2-digit GST state code",
	"gstin":            "invalid GSTIN format",
	"attrcode":         "must start with a letter and use only a-z, 0-9 and _",
	"gstrate":          "must be one of 0, 0.25, 3, 5, 12, 18, 28, 40",
	"date":             "must be a date like 2026-10-10",
	"uuid":             "must be a UUID",
	"gt":               "must be greater than %s",
	"gte":              "must be at least %s",
	"lte":              "must be at most %s",
	"dive":             "is invalid",
	"required_without": "is required when %s is not given",
}

// Validate checks struct tags and converts failures to a 400 problem.
func Validate(v any) error {
	err := validate.Struct(v)
	if err == nil {
		return nil
	}
	var verrs validator.ValidationErrors
	if !errors.As(err, &verrs) {
		return &Problem{Status: http.StatusBadRequest, Code: CodeValidation, Detail: err.Error()}
	}
	out := make([]FieldError, 0, len(verrs))
	for _, fe := range verrs {
		ns := fe.Namespace()
		if _, rest, ok := strings.Cut(ns, "."); ok {
			ns = rest
		}
		msg := messages[fe.Tag()]
		if msg == "" {
			msg = "is invalid (" + fe.Tag() + ")"
		}
		if strings.Contains(msg, "%s") {
			msg = fmt.Sprintf(msg, strings.ReplaceAll(fe.Param(), " ", ", "))
		}
		out = append(out, FieldError{Field: ns, Message: msg})
	}
	return ValidationErrors(out)
}

// ReadBody reads a JSON body (max 2 MiB). Used directly when the raw bytes
// are also needed (idempotency hash).
func ReadBody(r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return nil, &Problem{Status: http.StatusBadRequest, Code: CodeValidation, Detail: "could not read body"}
	}
	if len(b) > maxBody {
		return nil, &Problem{Status: http.StatusRequestEntityTooLarge, Code: CodeValidation, Detail: "body too large"}
	}
	return b, nil
}

// Decode reads, unmarshals and validates a JSON body into v.
func Decode(r *http.Request, v any) error {
	b, err := ReadBody(r)
	if err != nil {
		return err
	}
	return DecodeBytes(b, v)
}

// DecodeBytes unmarshals and validates JSON. An empty body is "{}".
func DecodeBytes(b []byte, v any) error {
	if len(bytes.TrimSpace(b)) == 0 {
		b = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			msg := "has the wrong type (expected " + te.Type.String() + ")"
			if te.Type.Kind() == reflect.String {
				msg = "must be a string; send decimals as strings like \"12.50\""
			}
			return Validation(te.Field, msg)
		}
		return &Problem{Status: http.StatusBadRequest, Code: CodeValidation, Detail: "invalid JSON: " + err.Error()}
	}
	return Validate(v)
}

// ParseDate parses an optional YYYY-MM-DD string (already validated).
func ParseDate(s *string) *time.Time {
	if s == nil || *s == "" {
		return nil
	}
	t, err := time.Parse(time.DateOnly, *s)
	if err != nil {
		return nil
	}
	return &t
}
