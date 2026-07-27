// Package payload builds the request bodies scenarios send, deterministically.
//
// The old lab built them in JavaScript, inside the k6 script, from environment
// variables — which meant the exact bytes that were offered existed only in the
// generator's memory for the duration of the run. Two scenarios are explicitly designed
// to be compared record-for-record (006 sends JSON, 007 sends the identical records as
// CSV), and that comparison rested on two separate JS files agreeing with each other by
// inspection.
//
// So the bytes are built here, hashed, archived beside the cell, and handed to k6 as a
// file. What was offered is as much a part of a result as what came back.
//
// Building once and reusing is deliberate and load-bearing: serialising a megabyte of
// JSON per iteration would make the generator, not the runtime, the thing under test.
package payload

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Kind names a generator.
type Kind string

const (
	// OrderLines is scenario 002's order document: a customer, a channel and N line
	// items. The line count is what decides how much work each message is, because
	// the flow maps a foreach over them.
	OrderLines Kind = "order-lines"
	// JSONRecords is scenario 006's document, grown by record count until the
	// serialised body reaches a target size.
	JSONRecords Kind = "json-records"
	// CSVRecords is scenario 007's document: the same records as [JSONRecords],
	// written as CSV.
	//
	// Sized in records rather than bytes, and that is the whole point. CSV carries no
	// keys, braces or quotes, so it holds a record in roughly half the space; equal
	// bytes would mean unequal work and the two scenarios could not be compared.
	CSVRecords Kind = "csv-records"
)

// Spec describes a body to build. The zero value builds nothing, which is how a GET
// scenario declares itself.
type Spec struct {
	Kind Kind `yaml:"kind"`

	// Lines is the line-item count for [OrderLines].
	Lines int `yaml:"lines,omitempty"`
	// Bytes is the target serialised size for [JSONRecords].
	Bytes int `yaml:"bytes,omitempty"`
	// Records is the record count for [CSVRecords].
	Records int `yaml:"records,omitempty"`
}

// Empty reports whether this spec asks for nothing.
func (s Spec) Empty() bool { return s.Kind == "" }

// Body is a built payload together with everything needed to identify it later.
type Body struct {
	Kind Kind `json:"kind"`
	// Bytes is the body itself. It is written to the cell directory, not into the
	// result document.
	Bytes []byte `json:"-"`
	Size  int    `json:"size"`
	// SHA256 identifies the exact bytes offered. Two campaigns claiming to compare
	// the same workload can be checked rather than assumed.
	SHA256 string `json:"sha256"`
	// Records is how many records or line items the body actually carries. For a
	// size-targeted payload this is the number that was measured rather than asked
	// for, and it is what makes a per-record cost meaningful.
	Records int `json:"records"`
}

// Build produces the body a spec describes.
func Build(s Spec) (Body, error) {
	var (
		b       []byte
		records int
		err     error
	)
	switch s.Kind {
	case "":
		return Body{}, nil
	case OrderLines:
		b, records, err = orderLines(s.Lines)
	case JSONRecords:
		b, records, err = jsonRecords(s.Bytes)
	case CSVRecords:
		b, records, err = csvRecords(s.Records)
	default:
		return Body{}, fmt.Errorf("payload: unknown kind %q", s.Kind)
	}
	if err != nil {
		return Body{}, err
	}
	sum := sha256.Sum256(b)
	return Body{
		Kind:    s.Kind,
		Bytes:   b,
		Size:    len(b),
		SHA256:  hex.EncodeToString(sum[:]),
		Records: records,
	}, nil
}

// --- scenario 002 ------------------------------------------------------------------

type orderLine struct {
	SKU   string  `json:"sku"`
	Qty   float64 `json:"qty"`
	Price float64 `json:"price"`
}

type order struct {
	Customer string      `json:"customer"`
	Channel  string      `json:"channel"`
	Lines    []orderLine `json:"lines"`
}

func orderLines(n int) ([]byte, int, error) {
	if n <= 0 {
		return nil, 0, fmt.Errorf("payload: %s needs a positive line count", OrderLines)
	}
	o := order{Customer: "acme-industrial", Channel: "web", Lines: make([]orderLine, n)}
	for i := range o.Lines {
		// Written as decimals because CEL will not multiply an int by a double, and
		// the flow multiplies qty by price. JSON has one number type, so what matters
		// is that the runtime's decoder produces doubles — which it does — not how
		// the digits are spelled here.
		o.Lines[i] = orderLine{
			SKU:   fmt.Sprintf("SKU-%d", 1000+i),
			Qty:   float64(i%4) + 1,
			Price: 5.5 + float64(i)*3.25,
		}
	}
	b, err := json.Marshal(o)
	if err != nil {
		return nil, 0, fmt.Errorf("payload: %w", err)
	}
	return b, n, nil
}

// --- scenarios 006 and 007 ---------------------------------------------------------

// The two scenarios exist to be compared, so they share one record definition. A
// separate JSON builder and CSV builder that merely looked equivalent is exactly the
// arrangement that lets them drift apart without anybody noticing.
var (
	regions    = []string{"us-east-1", "eu-west-1", "ap-south-1", "sa-east-1"}
	countries  = []string{"US", "IE", "IN", "BR"}
	currencies = []string{"USD", "EUR", "INR", "BRL"}
)

type record struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Region   string  `json:"region"`
	Country  string  `json:"country"`
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
	Status   string  `json:"status"`
}

func recordAt(i int) record {
	status := "ACTIVE"
	if i%7 == 0 {
		status = "CLOSED"
	}
	return record{
		ID:       fmt.Sprintf("acct-%08d", i),
		Name:     fmt.Sprintf("Account Holder %d", i),
		Region:   regions[i%len(regions)],
		Country:  countries[i%len(countries)],
		Amount:   float64(i%100) * 137.5,
		Currency: currencies[i%len(currencies)],
		Status:   status,
	}
}

const jsonRecordsCap = 200000

// jsonRecords grows the record count until the serialised document reaches target.
//
// Records are marshalled individually and their lengths accumulated rather than
// re-marshalling the whole document on each step. Re-marshalling is the obvious
// implementation and it is quadratic: the megabyte rung holds about 7,700 records, so
// the obvious version does gigabytes of work to build one body.
func jsonRecords(target int) ([]byte, int, error) {
	if target <= 0 {
		return nil, 0, fmt.Errorf("payload: %s needs a positive byte target", JSONRecords)
	}

	const (
		prefix = `{"records":[`
		suffix = `]}`
	)
	var (
		parts []string
		size  = len(prefix) + len(suffix)
	)
	for i := 0; size < target; i++ {
		if i >= jsonRecordsCap {
			return nil, 0, fmt.Errorf(
				"payload: %s reached %d records at %d bytes without meeting a %d-byte target",
				JSONRecords, i, size, target)
		}
		b, err := json.Marshal(recordAt(i))
		if err != nil {
			return nil, 0, fmt.Errorf("payload: %w", err)
		}
		if len(parts) > 0 {
			size++ // the comma
		}
		size += len(b)
		parts = append(parts, string(b))
	}

	var sb strings.Builder
	sb.Grow(size)
	sb.WriteString(prefix)
	sb.WriteString(strings.Join(parts, ","))
	sb.WriteString(suffix)
	return []byte(sb.String()), len(parts), nil
}

// csvHeader names the columns, in the same order as the JSON record's fields.
const csvHeader = "id,name,region,country,amount,currency,status"

func csvRecords(n int) ([]byte, int, error) {
	if n <= 0 {
		return nil, 0, fmt.Errorf("payload: %s needs a positive record count", CSVRecords)
	}
	var sb strings.Builder
	sb.WriteString(csvHeader)
	for i := 0; i < n; i++ {
		r := recordAt(i)
		sb.WriteString("\n")
		// No field contains a comma or a quote by construction, which is what lets
		// the scenario's CEL-written reader be correct. A generator that started
		// emitting one would silently change what the scenario measures, so the
		// shape is fixed here rather than configurable.
		sb.WriteString(strings.Join([]string{
			r.ID, r.Name, r.Region, r.Country,
			strconv.FormatFloat(r.Amount, 'g', -1, 64),
			r.Currency, r.Status,
		}, ","))
	}
	return []byte(sb.String()), n, nil
}
