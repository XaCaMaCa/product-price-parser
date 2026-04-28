package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type cfg struct {
	ProxyURL         string
	LocalProxy       bool
	LocalProxyAddr   string
	City             string
	StoreID          int
	Categories       []string
	OutFile          string
	PerCategory      int
	Timeout          time.Duration
	Offline          bool
	FixturesPath     string
	ExtraCookie      string
	CookieFile       string
	ForceHTTP11      bool
	ChromeTLS        bool
	WarmupWithCookie bool
	OmniwebHeaders   bool
	APIReferer       string
	MinimalJSONL     bool
}

type Store struct {
	ID      int
	Name    string
	Address string
	City    string
	Lat     float64
	Lon     float64
}

type Product struct {
	Category string `json:"category"`
	Name     string `json:"name"`
	Price    string `json:"price"`
	URL      string `json:"url"`
}

func main() {
	c := readFlags()
	if err := run(c); err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		os.Exit(1)
	}
}

func readFlags() cfg {
	var categoriesCSV string
	c := cfg{}
	flag.StringVar(&c.ProxyURL, "proxy", "", "внешний прокси, например http://user:pass@host:port")
	flag.BoolVar(&c.LocalProxy, "local-proxy", false, "локальный HTTP-прокси на этой машине (без апстрима)")
	flag.StringVar(&c.LocalProxyAddr, "local-proxy-addr", "127.0.0.1:0", "адрес локального прокси, :0 — свободный порт")
	flag.StringVar(&c.City, "city", "Москва", "город для X-Domain и выбора магазина")
	flag.IntVar(&c.StoreID, "store-id", 0, "id магазина, если известен")
	flag.StringVar(&categoriesCSV, "categories", "молочная продукция,хлебобулочные изделия,milk,bread", "категории через запятую")
	flag.StringVar(&c.OutFile, "out", "products.jsonl", "файл JSONL")
	flag.IntVar(&c.PerCategory, "per-category", 50, "лимит товаров на категорию")
	flag.DurationVar(&c.Timeout, "timeout", 25*time.Second, "таймаут HTTP")
	flag.BoolVar(&c.Offline, "offline", false, "только fixtures, без сети")
	flag.StringVar(&c.FixturesPath, "fixtures", "fixtures", "каталог с JSON-фикстурами")
	flag.StringVar(&c.ExtraCookie, "cookie", "", "заголовок Cookie с lenta.com (против Qrator)")
	flag.StringVar(&c.CookieFile, "cookie-file", "", "одна строка Cookie; перекрывает -cookie")
	flag.BoolVar(&c.ForceHTTP11, "http11", false, "только HTTP/1.1")
	flag.BoolVar(&c.ChromeTLS, "chrome-tls", false, "uTLS Chrome через прокси CONNECT")
	flag.BoolVar(&c.WarmupWithCookie, "warmup-with-cookie", false, "GET витрины с cookie и merge Set-Cookie")
	flag.BoolVar(&c.OmniwebHeaders, "omniweb-headers", false, "X-Platform, Sessiontoken и т.д.")
	flag.StringVar(&c.APIReferer, "api-referer", "", "Referer для /api; иначе …/{city}/")
	flag.BoolVar(&c.MinimalJSONL, "minimal-jsonl", false, "в файле только name, price, url")
	flag.Parse()

	for _, raw := range strings.Split(categoriesCSV, ",") {
		v := strings.TrimSpace(strings.ToLower(raw))
		if v != "" {
			c.Categories = append(c.Categories, v)
		}
	}
	if c.Timeout <= 0 {
		c.Timeout = 60 * time.Second
	}
	return c
}

func run(c cfg) error {
	if p := strings.TrimSpace(c.CookieFile); p != "" {
		p = filepath.Clean(p)
		b, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("cookie-file %q: %w", p, err)
		}
		cookie, err := normalizeCookieFileContent(b)
		if err != nil {
			return err
		}
		c.ExtraCookie = cookie
	}
	if c.LocalProxy && c.ProxyURL != "" {
		return errors.New("нельзя -local-proxy и -proxy вместе")
	}
	if c.LocalProxy && c.Offline {
		return errors.New("нельзя -offline и -local-proxy")
	}
	if c.ChromeTLS && c.Offline {
		return errors.New("нельзя -offline и -chrome-tls")
	}
	if c.LocalProxy {
		stop, u, err := startLocalForwardProxy(c.LocalProxyAddr)
		if err != nil {
			return fmt.Errorf("локальный прокси: %w", err)
		}
		defer stop()
		c.ProxyURL = u
		fmt.Fprintf(os.Stderr, "локальный прокси: %s\n", c.ProxyURL)
	}
	if c.ChromeTLS {
		fmt.Fprintln(os.Stderr, "режим uTLS через прокси")
		if c.ForceHTTP11 {
			fmt.Fprintln(os.Stderr, "заметка: при -chrome-tls -http11 на ALPN не влияет")
		}
	}
	if c.ProxyURL == "" && !c.Offline {
		return errors.New("нужен -proxy или -local-proxy, либо -offline")
	}

	var (
		store Store
		raws  []map[string]any
		err   error
	)
	if c.Offline {
		store, raws, err = loadOffline(c)
	} else {
		client, err2 := newHTTPClient(c)
		if err2 != nil {
			return err2
		}
		api := newLentaAPI(client, c.ExtraCookie, mapCityToXDomain(c.City), c.OmniwebHeaders, c.APIReferer)
		ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
		defer cancel()
		if strings.TrimSpace(c.ExtraCookie) == "" {
			wctx, wcancel := context.WithTimeout(ctx, 20*time.Second)
			if okURL, werr := api.warmUp(wctx); werr != nil {
				fmt.Fprintf(os.Stderr, "warning: прогрев: %v\n", werr)
			} else if u, e := url.Parse(okURL); e == nil && u.Host != "" {
				api.preferredAPIHost = u.Host
			}
			wcancel()
		} else if c.WarmupWithCookie {
			wctx, wcancel := context.WithTimeout(ctx, 20*time.Second)
			okURL, werr := api.warmUp(wctx)
			if werr != nil {
				fmt.Fprintf(os.Stderr, "warning: прогрев с cookie: %v\n", werr)
			} else {
				if merged := mergeCookieHeaderWithJar(c.ExtraCookie, client.Jar, okURL); merged != c.ExtraCookie {
					c.ExtraCookie = merged
					api = newLentaAPI(client, merged, mapCityToXDomain(c.City), c.OmniwebHeaders, c.APIReferer)
					fmt.Fprintln(os.Stderr, "прогрев: в Cookie добавлены пары из jar")
				}
				if u, e := url.Parse(okURL); e == nil && u.Host != "" {
					api.preferredAPIHost = u.Host
				}
			}
			wcancel()
		} else {
			fmt.Fprintln(os.Stderr, "прогрев отключён (задан cookie). При 404 на API попробуйте -warmup-with-cookie")
		}
		store, err = resolveStore(ctx, api, c)
		if err != nil {
			return err
		}
		raws, err = api.LoadRawCatalog(ctx, store.ID)
	}
	if err != nil {
		return err
	}
	items := extractProducts(raws, c.PerCategory)
	filtered := filterByCategories(items, c.Categories)
	if len(filtered) == 0 {
		fmt.Fprintln(os.Stderr, "warning: по категориям пусто, пишу всё, что извлечено")
		filtered = items
	}
	if err := writeJSONL(c.OutFile, filtered, c.MinimalJSONL); err != nil {
		return err
	}
	fmt.Printf("магазин: %s (id=%d)\n", store.Name, store.ID)
	fmt.Printf("записей: %d\n", len(filtered))
	fmt.Printf("файл: %s\n", c.OutFile)
	if !c.Offline && len(filtered) == 0 {
		fmt.Fprintln(os.Stderr, "онлайн 0 записей: часто Qrator (401/404) или пустой ответ API — обновить cookie, -proxy, -offline для проверки.")
	}
	return nil
}

func extractProducts(raws []map[string]any, perCategory int) []Product {
	var all []Product
	for _, root := range raws {
		walkJSON(root, "", &all)
	}
	seen := map[string]struct{}{}
	catCount := map[string]int{}
	var out []Product
	for _, p := range all {
		if p.Name == "" || p.Price == "" || p.URL == "" {
			continue
		}
		key := p.Name + "|" + p.URL
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		cat := strings.ToLower(strings.TrimSpace(p.Category))
		if cat == "" {
			cat = "без категории"
		}
		if perCategory > 0 && catCount[cat] >= perCategory {
			continue
		}
		if perCategory > 0 {
			catCount[cat]++
		}
		p.Category = cat
		out = append(out, p)
	}
	return out
}

func categoryHintFromMap(v map[string]any, parent string) string {
	for _, key := range []string{"title", "category", "categoryName", "group", "section"} {
		s := strings.TrimSpace(toString(v[key]))
		if s == "" || len(s) > 120 {
			continue
		}
		return s
	}
	return parent
}

func walkJSON(node any, parentCategory string, out *[]Product) {
	switch v := node.(type) {
	case map[string]any:
		cat := categoryHintFromMap(v, parentCategory)
		name := firstNonEmpty(
			toString(v["name"]),
			toString(v["title"]),
			toString(v["productName"]),
		)
		price := firstNonEmpty(
			toString(v["price"]),
			toString(v["currentPrice"]),
			toString(v["finalPrice"]),
		)
		link := firstNonEmpty(
			toString(v["url"]),
			toString(v["link"]),
			toString(v["productUrl"]),
		)
		if name != "" && price != "" && link != "" {
			*out = append(*out, Product{
				Category: cat,
				Name:     cleanSpace(name),
				Price:    cleanSpace(price),
				URL:      absolutizeURL(link),
			})
		}
		for _, child := range v {
			walkJSON(child, cat, out)
		}
	case []any:
		for _, child := range v {
			walkJSON(child, parentCategory, out)
		}
	}
}

func filterByCategories(items []Product, categories []string) []Product {
	if len(categories) == 0 {
		return items
	}
	var out []Product
	for _, p := range items {
		hay := strings.ToLower(p.Category + " " + p.Name)
		for _, cat := range categories {
			if strings.Contains(hay, cat) || categoryAliasMatch(hay, cat) {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

func categoryAliasMatch(hay, cat string) bool {
	switch cat {
	case "молочная продукция", "молочка", "milk", "dairy":
		return strings.Contains(hay, "молоч") || strings.Contains(hay, "молоко") ||
			strings.Contains(hay, "кефир") || strings.Contains(hay, "йогурт") || strings.Contains(hay, "dairy") || strings.Contains(hay, "milk")
	case "хлебобулочные изделия", "хлеб", "bread", "bakery":
		return strings.Contains(hay, "хлеб") || strings.Contains(hay, "батон") || strings.Contains(hay, "булоч") || strings.Contains(hay, "bakery") || strings.Contains(hay, "bread")
	default:
		return false
	}
}

func writeJSONL(path string, items []Product, minimal bool) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	if minimal {
		for _, p := range items {
			if err := enc.Encode(struct {
				Name  string `json:"name"`
				Price string `json:"price"`
				URL   string `json:"url"`
			}{p.Name, p.Price, p.URL}); err != nil {
				return err
			}
		}
		return nil
	}
	for _, p := range items {
		if err := enc.Encode(p); err != nil {
			return err
		}
	}
	return nil
}

func loadOffline(c cfg) (Store, []map[string]any, error) {
	store := Store{ID: 101, Name: "Лента (демо)", City: c.City, Address: "демо-адрес"}
	files := []string{"stores.json", "home.json", "mobilepromo.json"}
	var raws []map[string]any
	for _, name := range files {
		full := filepath.Join(c.FixturesPath, name)
		b, err := os.ReadFile(full)
		if err != nil {
			return Store{}, nil, err
		}
		var j map[string]any
		if err := json.Unmarshal(b, &j); err != nil {
			return Store{}, nil, err
		}
		raws = append(raws, j)
	}
	return store, raws, nil
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	default:
		return ""
	}
}

func toInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(math.Round(x))
	case int:
		return x
	case string:
		n, _ := strconv.Atoi(x)
		return n
	default:
		return 0
	}
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case string:
		n, _ := strconv.ParseFloat(x, 64)
		return n
	default:
		return 0
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func cleanSpace(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func urlTrimTrailingJunk(s string) string {
	return strings.TrimRight(s, "| \t\r\n>\"'")
}

func absolutizeURL(raw string) string {
	raw = urlTrimTrailingJunk(strings.TrimSpace(raw))
	var out string
	switch {
	case strings.HasPrefix(raw, "//"):
		out = "https:" + raw
	case strings.HasPrefix(raw, "/"):
		out = "https://lenta.com" + raw
	default:
		out = raw
	}
	return urlTrimTrailingJunk(out)
}
