package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
)

const lentaHostWWW, lentaHostApex = "www.lenta.com", "lenta.com"

type lentaAPI struct {
	http               *http.Client
	extraCookie        string
	userAgent          string
	xDomain            string
	omniwebHeaders     bool
	apiRefererOverride string
	preferredAPIHost   string
}

func newLentaAPI(client *http.Client, extraCookie, xDomain string, omniwebHeaders bool, apiReferer string) *lentaAPI {
	if strings.TrimSpace(xDomain) == "" {
		xDomain = "moscow"
	}
	return &lentaAPI{
		http:               client,
		extraCookie:        extraCookie,
		userAgent:          resolveUserAgentFromCookie(extraCookie),
		xDomain:            xDomain,
		omniwebHeaders:     omniwebHeaders,
		apiRefererOverride: strings.TrimSpace(apiReferer),
	}
}

// Если прогрев дал 200 с одного хоста — ходим только на него, чтобы не путать 401/404 qauth.
func (a *lentaAPI) apiHostsForRequests() []string {
	switch strings.ToLower(strings.TrimSpace(a.preferredAPIHost)) {
	case lentaHostApex:
		return []string{lentaHostApex}
	case lentaHostWWW:
		return []string{lentaHostWWW}
	default:
		return []string{lentaHostWWW, lentaHostApex}
	}
}

var xDomainByCity = []struct {
	sub, slug string
}{
	{"моск", "moscow"},
	{"петерб", "spb"}, {"спб", "spb"}, {"saint petersburg", "spb"},
	{"новосиб", "novosibirsk"},
	{"екатерин", "ekaterinburg"},
	{"казан", "kazan"},
	{"нижний", "nizhny-novgorod"},
	{"краснояр", "krasnoyarsk"},
	{"челябин", "chelyabinsk"},
	{"самар", "samara"},
	{"ростов", "rostov-on-don"},
	{"уфа", "ufa"},
	{"омск", "omsk"},
	{"воронеж", "voronezh"},
	{"перм", "perm"},
	{"волгоград", "volgograd"},
}

func mapCityToXDomain(city string) string {
	s := strings.ToLower(strings.TrimSpace(city))
	if s == "" {
		return "moscow"
	}
	for _, p := range xDomainByCity {
		if strings.Contains(s, p.sub) {
			return p.slug
		}
	}
	return "moscow"
}

var chromeMajorRE = regexp.MustCompile(`Chrome/(\d+)`)

func cookieValueFromExtra(extra, name string) string {
	if strings.TrimSpace(extra) == "" || name == "" {
		return ""
	}
	for _, part := range strings.Split(extra, ";") {
		key, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(key), name) {
			continue
		}
		val = strings.TrimSpace(val)
		if dec, err := url.QueryUnescape(val); err == nil {
			return dec
		}
		return val
	}
	return ""
}

func resolveUserAgentFromCookie(extra string) string {
	const fallback = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36"
	if ua := strings.TrimSpace(cookieValueFromExtra(extra, "User_Agent")); ua != "" {
		return ua
	}
	return fallback
}

func chromeMajorFromUA(ua string) string {
	m := chromeMajorRE.FindStringSubmatch(ua)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func originBaseFromURL(u *url.URL) string {
	if u == nil || u.Host == "" {
		return "https://www.lenta.com"
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return scheme + "://" + u.Host
}

func apiRefererPage(origin, xDomain string) string {
	o := strings.TrimSuffix(strings.TrimSpace(origin), "/")
	d := strings.Trim(strings.TrimSpace(xDomain), "/")
	if o == "" {
		o = "https://www.lenta.com"
	}
	if d == "" {
		d = "moscow"
	}
	return o + "/" + d + "/"
}

func (a *lentaAPI) warmUp(ctx context.Context) (string, error) {
	const maxBody = 2 << 20
	var candidates []string
	if strings.TrimSpace(a.apiRefererOverride) != "" {
		candidates = []string{a.apiRefererOverride}
	} else {
		candidates = []string{"https://www.lenta.com/", "https://lenta.com/"}
	}
	var lastErr error
	for _, page := range candidates {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, page, nil)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", page, err)
			continue
		}
		a.applyHeaders(req, "", false)
		resp, err := a.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", page, err)
			continue
		}
		_, copyErr := io.CopyN(io.Discard, resp.Body, maxBody)
		_ = resp.Body.Close()
		if copyErr != nil && copyErr != io.EOF {
			lastErr = fmt.Errorf("%s: %w", page, copyErr)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("%s: HTTP %d", page, resp.StatusCode)
			continue
		}
		return page, nil
	}
	if lastErr == nil {
		lastErr = errors.New("нет кандидатов")
	}
	return "", fmt.Errorf("ни одна витрина не ответила 200: %w", lastErr)
}

func (a *lentaAPI) applyHeaders(req *http.Request, referer string, api bool) {
	req.Header.Set("User-Agent", a.userAgent)
	if v := chromeMajorFromUA(a.userAgent); v != "" {
		req.Header.Set("Sec-CH-UA", fmt.Sprintf(`"Chromium";v="%s", "Google Chrome";v="%s", "Not=A?Brand";v="99"`, v, v))
		req.Header.Set("Sec-CH-UA-Mobile", "?0")
		req.Header.Set("Sec-CH-UA-Platform", `"Windows"`)
	}
	origin := originBaseFromURL(req.URL)
	if api {
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("Priority", "u=0, i")
		if a.omniwebHeaders {
			if tok := cookieValueFromExtra(a.extraCookie, "Utk_SessionToken"); tok != "" {
				req.Header.Set("Sessiontoken", tok)
			}
			if dev := cookieValueFromExtra(a.extraCookie, "Utk_DvcGuid"); dev != "" {
				req.Header.Set("Deviceid", dev)
				req.Header.Set("X-Device-Id", dev)
			}
			req.Header.Set("X-Platform", "omniweb")
			req.Header.Set("X-Retail-Brand", "lo")
			req.Header.Set("X-Domain", a.xDomain)
		}
		ref := apiRefererPage(origin, a.xDomain)
		if a.apiRefererOverride != "" {
			ref = a.apiRefererOverride
		}
		req.Header.Set("Referer", ref)
		req.Header.Set("Origin", origin)
		req.Header.Set("Sec-Fetch-Dest", "empty")
		req.Header.Set("Sec-Fetch-Mode", "cors")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	} else {
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		req.Header.Set("Origin", origin)
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Sec-Fetch-Site", "none")
	}
	req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7")
	if a.extraCookie != "" {
		req.Header.Set("Cookie", a.extraCookie)
	}
}

func apiErrorHint(status int, body, server string) string {
	low := strings.ToLower(body)
	if strings.Contains(low, "qauth.js") {
		return " (qauth)"
	}
	if status == http.StatusNotFound && strings.Contains(low, "page not found") {
		if server != "" {
			return " (сессия не принята, Server: " + server + ")"
		}
		return " (сессия/404)"
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		if strings.Contains(low, "qrator") {
			return " (WAF/qrator)"
		}
		return " (доступ)"
	}
	if status == http.StatusNotFound {
		return ""
	}
	return ""
}

func (a *lentaAPI) getJSON(ctx context.Context, link string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	a.applyHeaders(req, "", true)
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close() //nolint:errcheck
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		s := string(body)
		srv := strings.TrimSpace(resp.Header.Get("Server"))
		hint := apiErrorHint(resp.StatusCode, s, srv)
		return nil, fmt.Errorf("GET %s: HTTP %d%s, body=%s", link, resp.StatusCode, hint, truncate(s, 400))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (a *lentaAPI) ListStores(ctx context.Context) ([]Store, error) {
	var urls []string
	for _, h := range a.apiHostsForRequests() {
		urls = append(urls, "https://"+h+"/api/v1/stores/", "https://"+h+"/api/v1/stores")
	}
	var lastErr error
	for _, link := range urls {
		j, err := a.getJSON(ctx, link)
		if err != nil {
			lastErr = err
			continue
		}
		stores, err := parseStoresFromMap(j)
		if err != nil {
			lastErr = err
			continue
		}
		if len(stores) > 0 {
			return stores, nil
		}
		lastErr = errors.New("не удалось разобрать список магазинов")
	}
	if lastErr == nil {
		lastErr = errors.New("не удалось получить список магазинов")
	}
	return nil, lastErr
}

func parseStoresFromMap(j map[string]any) ([]Store, error) {
	var stores []Store
	candidates := []any{j["data"], j["stores"], j["items"]}
	for _, c := range candidates {
		arr, ok := c.([]any)
		if !ok {
			continue
		}
		for _, row := range arr {
			m, ok := row.(map[string]any)
			if !ok {
				continue
			}
			stores = append(stores, parseStore(m))
		}
	}
	if len(stores) == 0 {
		return nil, errors.New("не удалось разобрать список магазинов")
	}
	return stores, nil
}

func (a *lentaAPI) LoadRawCatalog(ctx context.Context, storeID int) ([]map[string]any, error) {
	hosts := a.apiHostsForRequests()
	tails := []string{
		fmt.Sprintf("/api/v1/stores/%d/home", storeID),
		fmt.Sprintf("/api/v1/stores/%d/mobilepromo?hideAlcohol=false&limit=500&offset=0&type=weekly", storeID),
		fmt.Sprintf("/api/v1/stores/%d/crazypromotions", storeID),
	}
	var links []string
	for _, h := range hosts {
		for _, t := range tails {
			links = append(links, "https://"+h+t)
		}
	}
	var out []map[string]any
	var fails []error
	for _, link := range links {
		j, err := a.getJSON(ctx, link)
		if err != nil {
			fails = append(fails, err)
			continue
		}
		out = append(out, j)
	}
	if len(out) == 0 && len(fails) > 0 {
		fmt.Fprintf(os.Stderr, "warning: все %d эндпоинтов отказали; первый: %v\n", len(fails), fails[0])
	} else {
		for _, err := range fails {
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("не удалось получить ни одного JSON каталога")
	}
	return out, nil
}

func tryPickupStoreIDFromCookie(cookie string) (int, bool) {
	cookie = strings.TrimSpace(cookie)
	if cookie == "" {
		return 0, false
	}
	const pfx = "App_Cache_MissionAddressMode="
	for _, part := range strings.Split(cookie, ";") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, pfx) {
			continue
		}
		raw := strings.TrimPrefix(part, pfx)
		dec, err := url.QueryUnescape(raw)
		if err != nil {
			dec = raw
		}
		var root struct {
			MA struct {
				I int `json:"i"`
			} `json:"ma"`
		}
		if err := json.Unmarshal([]byte(dec), &root); err != nil {
			return 0, false
		}
		if root.MA.I > 0 {
			return root.MA.I, true
		}
	}
	return 0, false
}

func resolveStore(ctx context.Context, api *lentaAPI, c cfg) (Store, error) {
	stores, err := api.ListStores(ctx)
	if err != nil {
		if c.StoreID > 0 {
			fmt.Fprintf(os.Stderr, "warning: /api/v1/stores недоступен (%v), -store-id=%d\n", err, c.StoreID)
			return Store{ID: c.StoreID, Name: fmt.Sprintf("магазин %d", c.StoreID), City: c.City}, nil
		}
		if id, ok := tryPickupStoreIDFromCookie(c.ExtraCookie); ok {
			fmt.Fprintf(os.Stderr, "warning: /api/v1/stores недоступен (%v), id из cookie=%d\n", err, id)
			return Store{ID: id, Name: fmt.Sprintf("магазин %d (cookie)", id), City: c.City}, nil
		}
		return Store{}, err
	}
	if c.StoreID > 0 {
		for _, s := range stores {
			if s.ID == c.StoreID {
				return s, nil
			}
		}
		return Store{}, fmt.Errorf("магазин id=%d не найден", c.StoreID)
	}
	needle := strings.ToLower(c.City)
	for _, s := range stores {
		if strings.Contains(strings.ToLower(s.City), needle) || strings.Contains(strings.ToLower(s.Address), needle) {
			return s, nil
		}
	}
	return stores[0], nil
}

func parseStore(m map[string]any) Store {
	return Store{
		ID:      toInt(m["id"]),
		Name:    toString(m["name"]),
		Address: toString(m["address"]),
		City:    toString(m["city"]),
		Lat:     toFloat(m["lat"]),
		Lon:     toFloat(m["lng"]),
	}
}
