// Arogga product search + Bangladesh gold prices as an MCP server. Serves /mcp on $PORT or 8000.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var client = &http.Client{Timeout: 30 * time.Second}

func getJSON(u string, headers map[string]string, out any) error {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", u, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ---- arogga ----

type SearchIn struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty" jsonschema:"max products, default 5, capped at 20"`
}

// ponytail: upstream fields are loosely typed (nulls, ints-as-bools), so pass them through as `any`.
type Product struct {
	Name                 any    `json:"name"`
	Generic              any    `json:"generic"`
	Form                 any    `json:"form"`
	Strength             any    `json:"strength"`
	Maker                any    `json:"maker"`
	Unit                 any    `json:"unit"`
	UnitsPerPack         any    `json:"units_per_pack"`
	MRPPerPackBDT        any    `json:"mrp_per_pack_bdt"`
	PricePerPackBDT      any    `json:"price_per_pack_bdt"`
	PricePerBaseUnitBDT  any    `json:"price_per_base_unit_bdt"`
	InStock              bool   `json:"in_stock"`
	PrescriptionRequired bool   `json:"prescription_required"`
	URL                  string `json:"url"`
}

type SearchOut struct {
	Results []Product `json:"results"`
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	}
	return true
}

func searchArogga(ctx context.Context, _ *mcp.CallToolRequest, in SearchIn) (*mcp.CallToolResult, SearchOut, error) {
	if in.Limit <= 0 {
		in.Limit = 5
	}
	q := url.Values{"_search": {in.Query}, "_perPage": {fmt.Sprint(min(in.Limit, 20))}, "_page": {"1"}}
	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := getJSON("https://api.arogga.com/general/v3/search/?"+q.Encode(), nil, &body); err != nil {
		return nil, SearchOut{}, err
	}
	out := SearchOut{Results: []Product{}}
	for _, p := range body.Data {
		pv, _ := p["pv"].([]any)
		for _, x := range pv[:min(len(pv), 3)] { // variants (pack sizes)
			v, _ := x.(map[string]any)
			out.Results = append(out.Results, Product{
				Name:                 p["p_name"],
				Generic:              p["p_generic_name"],
				Form:                 p["p_form"],
				Strength:             p["p_strength"],
				Maker:                p["p_brand_name"],
				Unit:                 v["pu_b2c_sales_unit_label"],
				UnitsPerPack:         v["pu_b2c_base_unit_multiplier"],
				MRPPerPackBDT:        v["pv_b2c_mrp"],
				PricePerPackBDT:      v["pv_b2c_price"],
				PricePerBaseUnitBDT:  v["pv_b2c_discounted_price"],
				InStock:              truthy(v["pv_stock_status"]),
				PrescriptionRequired: truthy(p["p_rx_req"]),
				URL:                  fmt.Sprintf("https://www.arogga.com/product/%v", p["id"]),
			})
		}
	}
	return nil, out, nil
}

// ---- gold ----

const bhori = 11.664 // grams

type goldRow struct {
	Date        string  `json:"date"`
	K22         float64 `json:"k22"`
	K21         float64 `json:"k21"`
	K18         float64 `json:"k18"`
	Traditional float64 `json:"traditional"`
}

func (r goldRow) karat(k string) (float64, bool) {
	switch k {
	case "22k":
		return r.K22, true
	case "21k":
		return r.K21, true
	case "18k":
		return r.K18, true
	case "traditional":
		return r.Traditional, true
	}
	return 0, false
}

var karats = []string{"22k", "21k", "18k", "traditional"}

func goldSense(path string, q url.Values, out any) error {
	var body struct {
		Data json.RawMessage `json:"data"`
	}
	u := "https://gold-sense.com/api/prices" + path
	if q != nil {
		u += "?" + q.Encode()
	}
	if err := getJSON(u, map[string]string{"Referer": "https://gold-sense.com/"}, &body); err != nil {
		return err
	}
	return json.Unmarshal(body.Data, out)
}

type GoldPricesOut struct {
	Date              string             `json:"date"`
	BDTPerGram        map[string]float64 `json:"bdt_per_gram"`
	BDTPerBhori       map[string]float64 `json:"bdt_per_bhori"`
	InternationalSpot map[string]float64 `json:"international_spot"`
	Note              string             `json:"note"`
}

func goldPrices(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, GoldPricesOut, error) {
	var row goldRow
	if err := goldSense("/latest", nil, &row); err != nil {
		return nil, GoldPricesOut{}, err
	}
	out := GoldPricesOut{
		Date:              row.Date,
		BDTPerGram:        map[string]float64{},
		BDTPerBhori:       map[string]float64{},
		InternationalSpot: map[string]float64{},
		Note:              "5% VAT and making charges are extra on jewellery",
	}
	for _, k := range karats {
		v, _ := row.karat(k)
		out.BDTPerGram[k] = v
		out.BDTPerBhori[k] = math.Round(v * bhori)
	}
	for name, sym := range map[string]string{"gold_usd_per_oz": "XAU", "silver_usd_per_oz": "XAG"} {
		var spot struct {
			Price float64 `json:"price"`
		}
		if err := getJSON("https://api.gold-api.com/price/"+sym, nil, &spot); err != nil {
			return nil, GoldPricesOut{}, err
		}
		out.InternationalSpot[name] = spot.Price
	}
	return nil, out, nil
}

type HistoryIn struct {
	Days  int    `json:"days,omitempty" jsonschema:"look-back window in days, default 30"`
	Karat string `json:"karat,omitempty" jsonschema:"22k, 21k, 18k or traditional; default 22k"`
}

type Point struct {
	Date       string  `json:"date"`
	BDTPerGram float64 `json:"bdt_per_gram"`
}

type GoldHistoryOut struct {
	Karat     string  `json:"karat"`
	From      string  `json:"from"`
	To        string  `json:"to"`
	Latest    float64 `json:"latest"`
	Min       float64 `json:"min"`
	Max       float64 `json:"max"`
	Change    float64 `json:"change"`
	ChangePct float64 `json:"change_pct"`
	Series    []Point `json:"series"`
}

func goldHistory(ctx context.Context, _ *mcp.CallToolRequest, in HistoryIn) (*mcp.CallToolResult, GoldHistoryOut, error) {
	if in.Days <= 0 {
		in.Days = 30
	}
	if in.Karat == "" {
		in.Karat = "22k"
	}
	in.Karat = strings.ToLower(in.Karat)
	if _, ok := (goldRow{}).karat(in.Karat); !ok {
		return nil, GoldHistoryOut{}, fmt.Errorf("karat must be one of %v", karats)
	}
	start := time.Now().UTC().AddDate(0, 0, -in.Days).Format("2006-01-02")
	var rows []goldRow
	q := url.Values{"startDate": {start}, "limit": {"-1"}, "sortBy": {"date"}, "sortOrder": {"desc"}}
	if err := goldSense("", q, &rows); err != nil {
		return nil, GoldHistoryOut{}, err
	}
	out := GoldHistoryOut{Karat: in.Karat, Series: []Point{}}
	seen := map[string]bool{}
	for _, r := range rows { // newest first; keep first row per day
		d := r.Date[:min(len(r.Date), 10)]
		if seen[d] {
			continue
		}
		seen[d] = true
		v, _ := r.karat(in.Karat)
		out.Series = append(out.Series, Point{d, v})
	}
	if len(out.Series) == 0 {
		return nil, out, fmt.Errorf("no data in range")
	}
	first, last := out.Series[len(out.Series)-1], out.Series[0]
	out.From, out.To, out.Latest = first.Date, last.Date, last.BDTPerGram
	out.Min, out.Max = last.BDTPerGram, last.BDTPerGram
	for _, p := range out.Series {
		out.Min, out.Max = min(out.Min, p.BDTPerGram), max(out.Max, p.BDTPerGram)
	}
	out.Change = last.BDTPerGram - first.BDTPerGram
	out.ChangePct = math.Round(out.Change/first.BDTPerGram*10000) / 100
	return nil, out, nil
}

func main() {
	s := mcp.NewServer(&mcp.Implementation{Name: "arogga", Version: "1.0.0"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "search_arogga", Description: "Search arogga.com (Bangladesh online pharmacy) for medicines/health products. " +
		"Returns name, generic, form/strength, maker, MRP and discounted price in BDT, and stock. " +
		"Prices are per base unit (e.g. per tablet) AND per sales unit (e.g. per strip)."}, searchArogga)
	mcp.AddTool(s, &mcp.Tool{Name: "gold_prices", Description: "Current Bangladesh gold price (BAJUS rates via gold-sense.com) in BDT, per gram and per " +
		"bhori/vori (11.664 g), for 22k/21k/18k/traditional. Also international spot (USD/oz)."}, goldPrices)
	mcp.AddTool(s, &mcp.Tool{Name: "gold_history", Description: "Bangladesh gold price history (BDT per gram), newest first, for the last `days` days. " +
		"karat: 22k, 21k, 18k or traditional. Includes min/max/change summary. Data goes back to Apr 2024."}, goldHistory)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}
	http.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil))
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	log.Printf("listening on :%s/mcp", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
