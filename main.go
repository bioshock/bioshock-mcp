// Arogga, Lazz Pharma + Shajgoj product search and Bangladesh gold prices as an MCP server. Serves /mcp on $PORT or 8000.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var client = &http.Client{Timeout: 30 * time.Second}

const cacheTTL = time.Hour

// ponytail: in-memory, per-process; expired entries swept on each write. Use Redis if you run >1 replica.
var cache = struct {
	sync.Mutex
	m map[string]cached
}{m: map[string]cached{}}

type cached struct {
	body []byte
	at   time.Time
}

func getJSON(u string, headers map[string]string, out any) error {
	return fetchJSON("GET", u, nil, headers, out)
}

// fetchJSON does every upstream call, cached for cacheTTL by method+URL+body. Only successful responses are cached.
func fetchJSON(method, u string, body []byte, headers map[string]string, out any) error {
	key := method + " " + u + " " + string(body)
	cache.Lock()
	c, ok := cache.m[key]
	cache.Unlock()
	if ok && time.Since(c.at) < cacheTTL {
		return json.Unmarshal(c.body, out)
	}

	req, err := http.NewRequest(method, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
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
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, out); err != nil {
		return err
	}
	cache.Lock()
	for k, c := range cache.m {
		if time.Since(c.at) >= cacheTTL {
			delete(cache.m, k)
		}
	}
	cache.m[key] = cached{b, time.Now()}
	cache.Unlock()
	return nil
}

// ---- currency ----

const fxSource = "open.er-api.com (ExchangeRate-API), updated daily"

var fxCurrencies = []string{"USD", "THB", "EUR"}

// price converts a BDT amount to BDT/USD/THB/EUR. If rates are unavailable it returns BDT only.
func price(bdt float64) map[string]float64 {
	m := map[string]float64{"BDT": bdt}
	var fx struct {
		Rates map[string]float64 `json:"rates"`
	}
	if getJSON("https://open.er-api.com/v6/latest/BDT", nil, &fx) != nil {
		return m
	}
	for _, c := range fxCurrencies {
		m[c] = math.Round(bdt*fx.Rates[c]*1e4) / 1e4
	}
	return m
}

// ---- arogga ----

type SearchIn struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty" jsonschema:"max products, default 5, capped at 20"`
}

// ponytail: upstream fields are loosely typed (nulls, ints-as-bools), so pass them through as `any`.
type Product struct {
	Name                 any                `json:"name"`
	Generic              any                `json:"generic"`
	Form                 any                `json:"form"`
	Strength             any                `json:"strength"`
	Maker                any                `json:"maker"`
	Unit                 any                `json:"unit"`
	UnitsPerPack         any                `json:"units_per_pack"`
	MRPPerPackBDT        any                `json:"mrp_per_pack_bdt"`
	PricePerPackBDT      any                `json:"price_per_pack_bdt"`
	PricePerBaseUnitBDT  any                `json:"price_per_base_unit_bdt"`
	InStock              bool               `json:"in_stock"`
	PrescriptionRequired bool               `json:"prescription_required"`
	Price                map[string]float64 `json:"price" jsonschema:"price per pack in BDT, USD, THB, EUR"`
	URL                  string             `json:"url"`
}

type SearchOut struct {
	Results []Product `json:"results"`
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
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
				Price:                price(num(v["pv_b2c_price"])),
			})
		}
	}
	return nil, out, nil
}

// ---- lazz pharma ----

type LazzIn struct {
	Query string `json:"query" jsonschema:"brand or generic name, e.g. napa or paracetamol"`
}

type LazzProduct struct {
	Name            string             `json:"name"`
	Generic         string             `json:"generic"`
	Form            string             `json:"form"`
	Strength        string             `json:"strength"`
	Maker           string             `json:"maker"`
	PricePerUnitBDT float64            `json:"price_per_unit_bdt"`
	DiscountPercent float64            `json:"discount_percent"`
	Stock           float64            `json:"stock"`
	InStock         bool               `json:"in_stock"`
	URL             string             `json:"url"`
	Price           map[string]float64 `json:"price" jsonschema:"price per unit in BDT, USD, THB, EUR"`
}

type LazzOut struct {
	Results []LazzProduct `json:"results"`
}

func searchLazz(ctx context.Context, _ *mcp.CallToolRequest, in LazzIn) (*mcp.CallToolResult, LazzOut, error) {
	// ponytail: upstream returns at most 7 hits regardless of PageSize.
	b, _ := json.Marshal(map[string]any{"PageNumber": 1, "PageSize": 50, "Query": strings.Fields(in.Query)})
	var body struct {
		IsError bool
		Msg     string
		Data    []struct {
			Name, GenericName, Strength, Type, SupplierName, Permalink string
			UnitSalePrice, DiscountPercent, TotalStock                 float64
		}
	}
	if err := fetchJSON("POST", "https://client.lazzpharma.com/ProductArea/Product/Search", b,
		map[string]string{"Referer": "https://www.lazzpharma.com/"}, &body); err != nil {
		return nil, LazzOut{}, err
	}
	if body.IsError {
		return nil, LazzOut{}, fmt.Errorf("lazzpharma: %s", body.Msg)
	}
	out := LazzOut{Results: []LazzProduct{}}
	for _, p := range body.Data {
		out.Results = append(out.Results, LazzProduct{
			Name:            p.Name,
			Generic:         strings.TrimSpace(p.GenericName),
			Form:            p.Type,
			Strength:        p.Strength,
			Maker:           p.SupplierName,
			PricePerUnitBDT: p.UnitSalePrice,
			DiscountPercent: p.DiscountPercent,
			Stock:           p.TotalStock,
			InStock:         p.TotalStock > 0,
			URL:             "https://www.lazzpharma.com/product/details/" + p.Permalink,
			Price:           price(p.UnitSalePrice),
		})
	}
	return nil, out, nil
}

// ---- shajgoj ----

type ShajgojIn struct {
	Query string `json:"query" jsonschema:"product, brand or category, e.g. sunscreen or cerave"`
	Page  int    `json:"page,omitempty" jsonschema:"results page (10 per page), default 1"`
}

type ShajgojProduct struct {
	Name            string             `json:"name"`
	Type            string             `json:"type"`
	Size            string             `json:"size,omitempty"`
	PriceBDT        float64            `json:"price_bdt"`
	RegularPriceBDT float64            `json:"regular_price_bdt"`
	PriceRange      string             `json:"price_range,omitempty"`
	OnSale          bool               `json:"on_sale"`
	SaleEnds        string             `json:"sale_ends,omitempty"`
	InStock         bool               `json:"in_stock"`
	Offers          []string           `json:"offers,omitempty"`
	URL             string             `json:"url"`
	Price           map[string]float64 `json:"price" jsonschema:"current price in BDT, USD, THB, EUR"`
}

type ShajgojOut struct {
	Total   int              `json:"total"`
	Page    int              `json:"page"`
	Results []ShajgojProduct `json:"results"`
}

func searchShajgoj(ctx context.Context, _ *mcp.CallToolRequest, in ShajgojIn) (*mcp.CallToolResult, ShajgojOut, error) {
	if in.Page <= 0 {
		in.Page = 1
	}
	q := url.Values{"s": {in.Query}, "page": {fmt.Sprint(in.Page)}}
	var body struct {
		NumFound int `json:"numFound"`
		Hits     []struct {
			Slug, Name, Type, Size string // Size can be null; decodes to ""
			PriceRange             string `json:"price_range"`
			SaleEndDate            string `json:"sale_end_date"`
			Price, Stock           float64
			SalePrice              float64  `json:"sale_price"`
			HasSale                bool     `json:"has_sale"`
			AvailableOffers        []string `json:"available_offers"`
		} `json:"hits"`
	}
	if err := getJSON("https://khoj.shajgoj.com/products?"+q.Encode(), nil, &body); err != nil {
		return nil, ShajgojOut{}, err
	}
	out := ShajgojOut{Total: body.NumFound, Page: in.Page, Results: []ShajgojProduct{}}
	for _, h := range body.Hits {
		p := ShajgojProduct{
			Name: h.Name, Type: h.Type, Size: h.Size, PriceRange: h.PriceRange,
			PriceBDT: h.Price, RegularPriceBDT: h.Price,
			InStock: h.Stock > 0, Offers: h.AvailableOffers,
			URL: "https://shop.shajgoj.com/product/" + h.Slug,
		}
		if h.HasSale {
			p.PriceBDT, p.OnSale, p.SaleEnds = h.SalePrice, true, h.SaleEndDate
		}
		p.Price = price(p.PriceBDT)
		out.Results = append(out.Results, p)
	}
	return nil, out, nil
}

// ---- gold ----

const bhori = 11.664 // grams

const goldSource = "gold-sense.com (cite this as the source)"

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
	Source            string             `json:"source"`
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
		Source:            goldSource,
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
	Source    string  `json:"source"`
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
	out := GoldHistoryOut{Source: goldSource, Karat: in.Karat, Series: []Point{}}
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

// ---- bazaar: all shops at once ----

type BazaarIn struct {
	Query string `json:"query" jsonschema:"product name as printed on the pack, e.g. 'Napa Extend 665' or 'CeraVe Foaming Cleanser'"`
}

type BazaarOut struct {
	Query      string            `json:"query"`
	Arogga     []Product         `json:"arogga"`
	LazzPharma []LazzProduct     `json:"lazzpharma"`
	Shajgoj    []ShajgojProduct  `json:"shajgoj"`
	Errors     map[string]string `json:"errors,omitempty"`
	FXSource   string            `json:"fx_source"`
}

func bazaar(ctx context.Context, req *mcp.CallToolRequest, in BazaarIn) (*mcp.CallToolResult, BazaarOut, error) {
	var (
		wg         sync.WaitGroup
		a          SearchOut
		l          LazzOut
		s          ShajgojOut
		ae, le, se error
	)
	wg.Go(func() { _, a, ae = searchArogga(ctx, req, SearchIn{Query: in.Query}) })
	wg.Go(func() { _, l, le = searchLazz(ctx, req, LazzIn{Query: in.Query}) })
	wg.Go(func() { _, s, se = searchShajgoj(ctx, req, ShajgojIn{Query: in.Query}) })
	wg.Wait()
	// one shop being down shouldn't sink the whole search
	out := BazaarOut{Query: in.Query, Arogga: a.Results, LazzPharma: l.Results, Shajgoj: s.Results, Errors: map[string]string{}, FXSource: fxSource}
	for site, err := range map[string]error{"arogga": ae, "lazzpharma": le, "shajgoj": se} {
		if err != nil {
			out.Errors[site] = err.Error()
		}
	}
	return nil, out, nil
}

func main() {
	s := mcp.NewServer(&mcp.Implementation{Name: "arogga", Version: "1.0.0"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "bazaar", Title: "Bazaar: search Bangladesh shops", Description: "Find a product and its price across Bangladesh online shops in one call: " +
		"arogga.com and lazzpharma.com (pharmacies: medicines, health products) and shop.shajgoj.com (cosmetics, skincare, beauty). " +
		"Use this first whenever the user asks where to buy something in Bangladesh or what it costs, or sends a photo of a product. " +
		"For a photo, read the brand, product name and strength/size off the packaging and pass that as `query`. " +
		"If nothing matches, retry with a shorter query (brand + product, or the generic name). " +
		"Prices are BDT; arogga/lazzpharma prices may be per unit (tablet) vs per pack, so compare like for like. " +
		"Each product has `price` in BDT, USD, THB and EUR (daily rates). " +
		"Results are grouped by shop; shops return loose matches, so drop unrelated items. `errors` lists any shop that failed."}, bazaar)
	mcp.AddTool(s, &mcp.Tool{Name: "search_arogga", Description: "Search arogga.com (Bangladesh online pharmacy) for medicines/health products. " +
		"Returns name, generic, form/strength, maker, MRP and discounted price in BDT, and stock. " +
		"Prices are per base unit (e.g. per tablet) AND per sales unit (e.g. per strip)."}, searchArogga)
	mcp.AddTool(s, &mcp.Tool{Name: "search_lazzpharma", Description: "Search lazzpharma.com (Lazz Pharma, Bangladesh online pharmacy) for medicines/health products " +
		"by brand or generic name. Returns up to 7 matches with form, strength, maker, price per unit (e.g. per tablet) in BDT, and stock."}, searchLazz)
	mcp.AddTool(s, &mcp.Tool{Name: "search_shajgoj", Description: "Search shop.shajgoj.com (Shajgoj, Bangladesh cosmetics/beauty/skincare shop) for products. " +
		"Returns 10 per page with current price and regular price in BDT, sale status, size, stock and offers. Use `page` for more."}, searchShajgoj)
	mcp.AddTool(s, &mcp.Tool{Name: "gold_prices", Description: "Current Bangladesh gold price from gold-sense.com, in BDT, per gram and per " +
		"bhori/vori (11.664 g), for 22k/21k/18k/traditional. Also international spot (USD/oz)."}, goldPrices)
	mcp.AddTool(s, &mcp.Tool{Name: "gold_history", Description: "Bangladesh gold price history from gold-sense.com (BDT per gram), newest first, for the last `days` days. " +
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
