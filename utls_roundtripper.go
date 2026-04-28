package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
)

// chromeDirectBypassNotice — однократное сообщение о прямом TCP в режиме chrome-tls + loopback-прокси.
var chromeDirectBypassNotice sync.Once

// prefixConn отдаёт сначала буфер после CONNECT-ответа, затем базовое соединение.
type prefixConn struct {
	prefix []byte
	net.Conn
}

func (p *prefixConn) Read(b []byte) (int, error) {
	if len(p.prefix) > 0 {
		n := copy(b, p.prefix)
		p.prefix = p.prefix[n:]
		return n, nil
	}
	return p.Conn.Read(b)
}

// utlsBody закрывает TCP после чтения тела ответа.
type utlsBody struct {
	io.ReadCloser
	tcp net.Conn
}

func (b *utlsBody) Close() error {
	err := b.ReadCloser.Close()
	_ = b.tcp.Close()
	return err
}

// utlsProxyRT — HTTPS с ClientHello как у Chrome (uTLS). Либо CONNECT через HTTP-прокси, либо прямой TCP (см. directLoopback).
type utlsProxyRT struct {
	proxy *url.URL
	// directLoopback: прокси на 127.0.0.1/localhost — CONNECT к самому себе на части систем даёт вечный dial timeout;
	// тогда TCP к целевому хосту идёт напрямую (uTLS по-прежнему Chrome).
	directLoopback bool
}

func newUTLSProxyRoundTripper(proxy *url.URL) http.RoundTripper {
	return &utlsProxyRT{proxy: proxy, directLoopback: isLoopbackHTTPProxy(proxy)}
}

func isLoopbackHTTPProxy(u *url.URL) bool {
	if u == nil {
		return false
	}
	switch u.Scheme {
	case "http", "https", "":
	default:
		return false
	}
	h := u.Hostname()
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

func (t *utlsProxyRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil || req.URL.Scheme != "https" {
		return nil, fmt.Errorf("utls: только https, получено %v", req.URL)
	}
	ctx := req.Context()
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		req = req.WithContext(ctx)
	}

	targetHost := req.URL.Hostname()
	targetPort := req.URL.Port()
	if targetPort == "" {
		targetPort = "443"
	}
	targetAddr := net.JoinHostPort(targetHost, targetPort)

	d := net.Dialer{Timeout: 30 * time.Second}

	var raw net.Conn
	var prefix []byte

	if t.directLoopback {
		chromeDirectBypassNotice.Do(func() {
			fmt.Fprintln(os.Stderr, "warning: -chrome-tls: исходящие HTTPS к lenta.com идут TCP напрямую (без CONNECT через локальный прокси), иначе на этой системе dial к 127.0.0.1 таймаутится. Локальный прокси остаётся запущенным.")
		})
		var err error
		raw, err = d.DialContext(ctx, "tcp", targetAddr)
		if err != nil {
			return nil, fmt.Errorf("utls-direct: dial %s: %w", targetAddr, err)
		}
		fmt.Fprintf(os.Stderr, "[utls-direct] %s %s\n", req.Method, targetAddr)
	} else {
		proxyAddr := t.proxy.Host
		if _, _, err := net.SplitHostPort(proxyAddr); err != nil {
			if t.proxy.Scheme == "http" || t.proxy.Scheme == "" {
				proxyAddr = net.JoinHostPort(proxyAddr, "80")
			}
		}
		if h, p, err := net.SplitHostPort(proxyAddr); err == nil {
			if ip := net.ParseIP(h); ip != nil && (ip.IsUnspecified() || ip.Equal(net.IPv6loopback)) {
				proxyAddr = net.JoinHostPort("127.0.0.1", p)
			}
		}
		var err error
		raw, err = d.DialContext(ctx, "tcp", proxyAddr)
		if err != nil {
			return nil, fmt.Errorf("utls-proxy: dial прокси: %w", err)
		}
		fmt.Fprintf(os.Stderr, "[utls-proxy] %s %s через прокси %s\n", req.Method, req.URL.Host, proxyAddr)

		connectHost := targetAddr
		var connect string
		if pa := proxyAuthHeader(t.proxy); pa != "" {
			connect = fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n", connectHost, connectHost, pa)
		} else {
			connect = fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", connectHost, connectHost)
		}
		if _, err := io.WriteString(raw, connect); err != nil {
			raw.Close()
			return nil, fmt.Errorf("utls-proxy: CONNECT: %w", err)
		}

		br := bufio.NewReader(raw)
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
		if err != nil {
			raw.Close()
			return nil, fmt.Errorf("utls-proxy: ответ CONNECT: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			raw.Close()
			return nil, fmt.Errorf("utls-proxy: CONNECT %s", resp.Status)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		if br.Buffered() > 0 {
			prefix = make([]byte, br.Buffered())
			if _, err := io.ReadFull(br, prefix); err != nil {
				raw.Close()
				return nil, fmt.Errorf("utls-proxy: буфер после CONNECT: %w", err)
			}
		}
	}

	_ = raw.SetDeadline(time.Now().Add(40 * time.Second))
	return utlsThenHTTP(ctx, raw, prefix, req)
}

func utlsThenHTTP(ctx context.Context, raw net.Conn, prefix []byte, req *http.Request) (*http.Response, error) {
	var pconn net.Conn
	if len(prefix) > 0 {
		pconn = &prefixConn{prefix: prefix, Conn: raw}
	} else {
		pconn = raw
	}

	cfg := &utls.Config{ServerName: req.URL.Hostname()}
	spec, err := buildChromeHTTP11ClientHelloSpec()
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("utls: spec: %w", err)
	}
	uconn := utls.UClient(pconn, cfg, utls.HelloCustom)
	if err := uconn.ApplyPreset(spec); err != nil {
		raw.Close()
		return nil, fmt.Errorf("utls: ApplyPreset: %w", err)
	}
	if err := uconn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("utls: TLS: %w", err)
	}
	fmt.Fprintln(os.Stderr, "[utls] TLS handshake OK")
	if np := uconn.ConnectionState().NegotiatedProtocol; np != "" && np != "http/1.1" {
		raw.Close()
		return nil, fmt.Errorf("utls: после рукопожатия ALPN=%q (ожидался http/1.1)", np)
	}

	req2 := req.Clone(ctx)
	req2.Close = true
	if err := req2.Write(uconn); err != nil {
		raw.Close()
		return nil, fmt.Errorf("utls: запись запроса: %w", err)
	}
	_ = raw.SetDeadline(time.Now().Add(40 * time.Second))

	res, err := http.ReadResponse(bufio.NewReader(uconn), req2)
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("utls: чтение ответа: %w", err)
	}
	if res.Body != nil {
		res.Body = &utlsBody{ReadCloser: res.Body, tcp: raw}
	} else {
		_ = raw.Close()
	}
	return res, nil
}

// buildChromeHTTP11ClientHelloSpec — Chrome-подобный ClientHello, ALPN только http/1.1.
func buildChromeHTTP11ClientHelloSpec() (*utls.ClientHelloSpec, error) {
	spec, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
	if err != nil {
		return nil, err
	}
	var filtered []utls.TLSExtension
	for _, ext := range spec.Extensions {
		if _, ok := ext.(*utls.ALPNExtension); ok {
			filtered = append(filtered, &utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}})
			continue
		}
		filtered = append(filtered, ext)
	}
	spec.Extensions = filtered
	return &spec, nil
}

func proxyAuthHeader(u *url.URL) string {
	if u.User == nil {
		return ""
	}
	user := u.User.Username()
	pw, _ := u.User.Password()
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pw))
}
