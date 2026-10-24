package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// printer writes a result as a table for people, or as JSON or CSV for
// scripts. JSON carries the API messages as protojson renders them.
type printer struct {
	w      io.Writer
	format string // table | json | csv
}

func newPrinter(w io.Writer, format string) (*printer, error) {
	switch format {
	case "", "table":
		return &printer{w: w, format: "table"}, nil
	case "json", "csv":
		return &printer{w: w, format: format}, nil
	}
	return nil, usageError{fmt.Errorf("-o %q: want table, json or csv", format)}
}

func (p *printer) json() bool { return p.format == "json" }

// data writes v, which may hold proto messages, as indented JSON.
func (p *printer) data(v any) error {
	b, err := toJSON(v)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, b, "", "  "); err != nil {
		return err
	}
	out.WriteByte('\n')
	_, err = p.w.Write(out.Bytes())
	return err
}

// toJSON marshals proto messages with protojson and everything else with
// encoding/json, nesting freely.
func toJSON(v any) ([]byte, error) {
	switch x := v.(type) {
	case proto.Message:
		return protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}.Marshal(x)
	case []any:
		parts := make([]json.RawMessage, len(x))
		for i, e := range x {
			b, err := toJSON(e)
			if err != nil {
				return nil, err
			}
			parts[i] = b
		}
		return json.Marshal(parts)
	case map[string]any:
		obj := make(map[string]json.RawMessage, len(x))
		for k, e := range x {
			b, err := toJSON(e)
			if err != nil {
				return nil, err
			}
			obj[k] = b
		}
		return json.Marshal(obj)
	}
	return json.Marshal(v)
}

func messages[T proto.Message](ms []T) []any {
	out := make([]any, len(ms))
	for i, m := range ms {
		out[i] = m
	}
	return out
}

// rows writes a table, or CSV with the same header in snake_case.
func (p *printer) rows(header []string, rows [][]string) error {
	if p.format == "csv" {
		w := csv.NewWriter(p.w)
		h := make([]string, len(header))
		for i, c := range header {
			h[i] = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(c), " ", "_"))
		}
		if err := w.Write(h); err != nil {
			return err
		}
		if err := w.WriteAll(rows); err != nil {
			return err
		}
		w.Flush()
		return w.Error()
	}
	tw := tabwriter.NewWriter(p.w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range rows {
		cells := make([]string, len(r))
		for i, c := range r {
			cells[i] = orDash(c)
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}

// usd is a cost for this format: $1.2345 in a table, exact dollars in CSV.
func (p *printer) usd(nano int64) string {
	if p.format == "csv" {
		return exactUSD(nano)
	}
	return fmt.Sprintf("$%.4f", float64(nano)/1e9)
}

// exactUSD writes nano-USD as dollars without float rounding: 36000000 is 0.036.
func exactUSD(nano int64) string {
	sign := ""
	if nano < 0 {
		sign, nano = "-", -nano
	}
	frac := strings.TrimRight(fmt.Sprintf("%09d", nano%1_000_000_000), "0")
	if frac == "" {
		return sign + strconv.FormatInt(nano/1_000_000_000, 10)
	}
	return sign + strconv.FormatInt(nano/1_000_000_000, 10) + "." + frac
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
