package payload

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAnEmptySpecBuildsNothing(t *testing.T) {
	// A GET scenario declares itself by saying nothing, and must not end up sending an
	// empty body with a content type attached.
	b, err := Build(Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Bytes) != 0 || b.Kind != "" || b.SHA256 != "" {
		t.Errorf("an empty spec produced %+v", b)
	}
}

func TestAnUnknownKindIsRefused(t *testing.T) {
	// Silently sending nothing would turn a typo into a scenario that measures an
	// empty POST and reports it as the workload.
	if _, err := Build(Spec{Kind: "orders"}); err == nil {
		t.Error("an unknown kind was accepted")
	}
}

func TestOrderLinesHasTheShapeTheFlowReads(t *testing.T) {
	b, err := Build(Spec{Kind: OrderLines, Lines: 8})
	if err != nil {
		t.Fatal(err)
	}

	var got struct {
		Customer string `json:"customer"`
		Channel  string `json:"channel"`
		Lines    []struct {
			SKU   string  `json:"sku"`
			Qty   float64 `json:"qty"`
			Price float64 `json:"price"`
		} `json:"lines"`
	}
	if err := json.Unmarshal(b.Bytes, &got); err != nil {
		t.Fatalf("the body is not the JSON the flow will parse: %v", err)
	}
	if got.Customer != "acme-industrial" || got.Channel != "web" {
		t.Errorf("wrong envelope: %+v", got)
	}
	if len(got.Lines) != 8 || b.Records != 8 {
		t.Fatalf("got %d lines, recorded %d, want 8", len(got.Lines), b.Records)
	}
	if got.Lines[0].SKU != "SKU-1000" || got.Lines[7].SKU != "SKU-1007" {
		t.Errorf("sku sequence moved: %q … %q", got.Lines[0].SKU, got.Lines[7].SKU)
	}
	// qty cycles 1..4 and price steps by 3.25. The flow multiplies the two, so both
	// have to stay non-zero or the scenario measures a multiplication by zero.
	if got.Lines[0].Qty != 1 || got.Lines[3].Qty != 4 || got.Lines[4].Qty != 1 {
		t.Errorf("qty does not cycle 1..4: %v", got.Lines)
	}
	if got.Lines[0].Price != 5.5 || got.Lines[1].Price != 8.75 {
		t.Errorf("price ladder moved: %v %v", got.Lines[0].Price, got.Lines[1].Price)
	}
}

func TestOrderLinesNeedsLines(t *testing.T) {
	if _, err := Build(Spec{Kind: OrderLines}); err == nil {
		t.Error("a zero line count was accepted; the scenario would measure an empty foreach")
	}
}

func TestJSONRecordsReachesItsTargetSizeAndNoFurther(t *testing.T) {
	// The ladder is the point of scenario 006: a rung has to be the size it claims, or
	// "cost against payload size" has no x axis.
	for _, target := range []int{1024, 102400, 1 << 20} {
		b, err := Build(Spec{Kind: JSONRecords, Bytes: target})
		if err != nil {
			t.Fatalf("target %d: %v", target, err)
		}
		if b.Size < target {
			t.Errorf("target %d produced %d bytes", target, b.Size)
		}
		// One record of overshoot is expected — the last one is what crosses the
		// line. Much more than that means the size accounting is not tracking the
		// document it eventually emits.
		if slack := b.Size - target; slack > 400 {
			t.Errorf("target %d overshot by %d bytes", target, slack)
		}
	}
}

// The incremental size accounting is the part that could silently disagree with the
// document it emits, so it is checked against the document rather than trusted.
func TestJSONRecordsAccountsForExactlyTheBytesItEmits(t *testing.T) {
	b, err := Build(Spec{Kind: JSONRecords, Bytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Records []map[string]any `json:"records"`
	}
	if err := json.Unmarshal(b.Bytes, &doc); err != nil {
		t.Fatalf("the body is not valid JSON: %v", err)
	}
	if len(doc.Records) != b.Records {
		t.Errorf("recorded %d records, document holds %d", b.Records, len(doc.Records))
	}
	if b.Size != len(b.Bytes) {
		t.Errorf("recorded size %d, actual %d", b.Size, len(b.Bytes))
	}
}

func TestCSVRecordsCarryTheSameRecordsAsTheJSON(t *testing.T) {
	// This is the invariant that lets 006 and 007 be drawn on one chart: at the same
	// record count they hold the same data, so what separates them is the input format
	// and nothing else. Two hand-written generators in two languages is precisely how
	// that stops being true without anybody noticing.
	const n = 9

	csv, err := Build(Spec{Kind: CSVRecords, Records: n})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(csv.Bytes), "\n")
	if len(lines) != n+1 {
		t.Fatalf("got %d lines, want a header and %d records", len(lines), n)
	}
	if lines[0] != "id,name,region,country,amount,currency,status" {
		t.Fatalf("header moved: %q", lines[0])
	}

	// Take the same records through the JSON generator and compare field by field.
	jsonBody, _, err := jsonRecordsCount(n)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Records []record `json:"records"`
	}
	if err := json.Unmarshal(jsonBody, &doc); err != nil {
		t.Fatal(err)
	}
	for i, r := range doc.Records {
		fields := strings.Split(lines[i+1], ",")
		if len(fields) != 7 {
			t.Fatalf("record %d has %d fields: %q", i, len(fields), lines[i+1])
		}
		if fields[0] != r.ID || fields[1] != r.Name || fields[2] != r.Region ||
			fields[3] != r.Country || fields[5] != r.Currency || fields[6] != r.Status {
			t.Errorf("record %d differs between the two formats:\n csv  %q\n json %+v",
				i, lines[i+1], r)
		}
	}
}

// jsonRecordsCount builds exactly n records, which the byte-targeted public API cannot
// express. Test-only, and only so the two formats can be compared at a fixed count.
func jsonRecordsCount(n int) ([]byte, int, error) {
	recs := make([]record, n)
	for i := range recs {
		recs[i] = recordAt(i)
	}
	b, err := json.Marshal(struct {
		Records []record `json:"records"`
	}{recs})
	return b, n, err
}

func TestNoCSVFieldNeedsQuoting(t *testing.T) {
	// The scenario's reader is written in CEL and splits on commas. A record that
	// contained one would not fail loudly; it would shift every column after it and
	// the transform would still return a document.
	b, err := Build(Spec{Kind: CSVRecords, Records: 500})
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(b.Bytes), "\n") {
		if strings.Count(line, ",") != 6 {
			t.Fatalf("line %d has %d commas, want 6: %q", i, strings.Count(line, ","), line)
		}
		if strings.ContainsAny(line, `"`+"\r") {
			t.Fatalf("line %d needs quoting: %q", i, line)
		}
	}
}

func TestTheSameSpecAlwaysProducesTheSameBytes(t *testing.T) {
	// The digest is only provenance if it is stable. Two campaigns claiming to compare
	// the same workload are checkable exactly to the extent that this holds.
	for _, s := range []Spec{
		{Kind: OrderLines, Lines: 8},
		{Kind: JSONRecords, Bytes: 1024},
		{Kind: CSVRecords, Records: 9},
	} {
		a, err := Build(s)
		if err != nil {
			t.Fatal(err)
		}
		b, err := Build(s)
		if err != nil {
			t.Fatal(err)
		}
		if a.SHA256 != b.SHA256 {
			t.Errorf("%s is not deterministic: %s vs %s", s.Kind, a.SHA256, b.SHA256)
		}
		if a.SHA256 == "" {
			t.Errorf("%s produced no digest", s.Kind)
		}
	}
}

func TestDifferentRungsAreDifferentBytes(t *testing.T) {
	small, err := Build(Spec{Kind: JSONRecords, Bytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	large, err := Build(Spec{Kind: JSONRecords, Bytes: 102400})
	if err != nil {
		t.Fatal(err)
	}
	if small.SHA256 == large.SHA256 {
		t.Error("two rungs of the ladder hash the same; the size parameter is not reaching the body")
	}
}
