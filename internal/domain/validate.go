package domain

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// FieldError is one problem at one JSON path, e.g. "line_items[2].quantity".
type FieldError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// ValidationErrors collects every problem instead of stopping at the first.
// On day 3 this list is fed back to Claude so it can fix its own output.
type ValidationErrors []FieldError

func (v ValidationErrors) Error() string {
	parts := make([]string, len(v))
	for i, e := range v {
		parts[i] = e.Path + ": " + e.Message
	}
	return strings.Join(parts, "; ")
}

func (v *ValidationErrors) add(path, format string, args ...any) {
	*v = append(*v, FieldError{Path: path, Message: fmt.Sprintf(format, args...)})
}

// Err returns nil when there are no problems, so callers can write
// `if err := x.Validate(); err != nil`.
func (v ValidationErrors) Err() error {
	if len(v) == 0 {
		return nil
	}
	return v
}

const maxQuoteLen = 300

var (
	reCurrency = regexp.MustCompile(`^[A-Z]{3}$`) // ISO 4217
	reCountry  = regexp.MustCompile(`^[A-Z]{2}$`) // ISO 3166-1 alpha-2
	reHSCode   = regexp.MustCompile(`^\d{6}(\d{2})?$`)
)

// Incoterms 2020.
var incoterms = map[string]bool{
	"EXW": true, "FCA": true, "CPT": true, "CIP": true, "DAP": true, "DPU": true,
	"DDP": true, "FAS": true, "FOB": true, "CFR": true, "CIF": true,
}

// checkMeta applies the rules every Field shares:
// a present value needs a valid confidence and evidence; a missing one needs neither.
func checkMeta[T any](errs *ValidationErrors, path string, f Field[T], required bool) {
	if !f.Present() {
		if required {
			errs.add(path, "is required")
		}
		return
	}
	if !f.Confidence.Valid() {
		errs.add(path+".confidence", "must be one of high, medium, low")
	}
	if f.Evidence == nil {
		errs.add(path+".evidence", "is required when value is present")
		return
	}
	if f.Evidence.Page < 1 {
		errs.add(path+".evidence.page", "must be >= 1")
	}
	q := strings.TrimSpace(f.Evidence.Quote)
	if q == "" {
		errs.add(path+".evidence.quote", "must not be empty")
	} else if len(q) > maxQuoteLen {
		errs.add(path+".evidence.quote", "must be at most %d characters", maxQuoteLen)
	}
}

func checkString(errs *ValidationErrors, path string, f Field[string], required bool, re *regexp.Regexp, hint string) {
	checkMeta(errs, path, f, required)
	if !f.Present() {
		return
	}
	v := strings.TrimSpace(*f.Value)
	if v == "" {
		errs.add(path, "must not be blank (use null if not found)")
		return
	}
	if re != nil && !re.MatchString(v) {
		errs.add(path, "must be %s", hint)
	}
}

func checkDate(errs *ValidationErrors, path string, f Field[string], required bool) {
	checkMeta(errs, path, f, required)
	if f.Present() {
		if _, err := time.Parse(time.DateOnly, *f.Value); err != nil {
			errs.add(path, "must be a date in YYYY-MM-DD format")
		}
	}
}

func checkInt(errs *ValidationErrors, path string, f Field[int64], required bool, min int64) {
	checkMeta(errs, path, f, required)
	if f.Present() && *f.Value < min {
		errs.add(path, "must be >= %d", min)
	}
}

func checkIncoterm(errs *ValidationErrors, path string, f Field[string]) {
	checkMeta(errs, path, f, false)
	if f.Present() && !incoterms[*f.Value] {
		errs.add(path, "must be an Incoterms 2020 code such as FOB, CIF or EXW")
	}
}
