package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
)

// После редиректа www↔apex у запроса уже другой Host — подставляем Origin/Referer под целевой URL, иначе 403/404.
func fixHeadersAfterLentaHostRedirect(req *http.Request, via []*http.Request, city string) {
	if len(via) == 0 || req.URL == nil {
		return
	}
	prev := via[len(via)-1].URL
	if prev == nil || prev.Host == req.URL.Host {
		return
	}
	ph, rh := strings.ToLower(prev.Host), strings.ToLower(req.URL.Host)
	if !strings.HasSuffix(ph, "lenta.com") || !strings.HasSuffix(rh, "lenta.com") {
		return
	}
	scheme := req.URL.Scheme
	if scheme == "" {
		scheme = "https"
	}
	newOrigin := scheme + "://" + req.URL.Host
	req.Header.Set("Origin", newOrigin)
	req.Header.Set("Referer", apiRefererPage(newOrigin, mapCityToXDomain(city)))
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-site")
}

func makeLentaCheckRedirect(c cfg) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 16 {
			return fmt.Errorf("слишком много редиректов")
		}
		fixHeadersAfterLentaHostRedirect(req, via, c.City)
		return nil
	}
}

func newHTTPClient(c cfg) (*http.Client, error) {
	proxyURL, err := url.Parse(c.ProxyURL)
	if err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	rd := makeLentaCheckRedirect(c)
	if c.ChromeTLS {
		return &http.Client{
			Timeout:       c.Timeout,
			Transport:     newUTLSProxyRoundTripper(proxyURL),
			Jar:           jar,
			CheckRedirect: rd,
		}, nil
	}
	tr := &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if c.ForceHTTP11 {
		tr.TLSNextProto = map[string]func(authority string, c *tls.Conn) http.RoundTripper{}
	}
	return &http.Client{
		Timeout:       c.Timeout,
		Transport:     tr,
		Jar:           jar,
		CheckRedirect: rd,
	}, nil
}

// Убираем BOM, запрещаем UTF-16 — одна строка Cookie.
func normalizeCookieFileContent(b []byte) (string, error) {
	if len(b) >= 2 {
		if b[0] == 0xFF && b[1] == 0xFE || b[0] == 0xFE && b[1] == 0xFF {
			return "", errors.New("cookie-file в UTF-16: пересохраните как UTF-8 (Блокнот → «Сохранить как»)")
		}
	}
	s := string(b)
	s = strings.TrimPrefix(s, "\ufeff")
	s = strings.TrimSpace(s)
	if strings.ContainsAny(s, "\r\n") {
		return "", errors.New("cookie-file: должна быть одна строка Cookie без переносов")
	}
	return s, nil
}

// net/http с ручным Cookie не мешает jar: дописываем пары из jar, которых ещё нет.
func mergeCookieHeaderWithJar(header string, jar http.CookieJar, pageURL string) string {
	u, err := url.Parse(pageURL)
	if err != nil || jar == nil {
		return header
	}
	existing := map[string]struct{}{}
	for _, part := range strings.Split(header, ";") {
		key, _, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok {
			existing[strings.TrimSpace(key)] = struct{}{}
		}
	}
	out := strings.TrimSpace(header)
	for _, ck := range jar.Cookies(u) {
		if ck == nil || ck.Name == "" {
			continue
		}
		if _, dup := existing[ck.Name]; dup {
			continue
		}
		if out != "" {
			out += "; "
		}
		out += ck.Name + "=" + ck.Value
		existing[ck.Name] = struct{}{}
	}
	return out
}
