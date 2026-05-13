package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// startLocalForwardProxy поднимает простой HTTP-прокси с поддержкой CONNECT (для HTTPS).
// Трафик уходит напрямую с вашей машины, без внешнего апстрима — но для клиента это всё равно «прокси».
func startLocalForwardProxy(addr string) (stop func(), proxyURL string, err error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", err
	}
	go acceptProxyLoop(ln)
	stop = func() {
		_ = ln.Close() //nolint:errcheck
	}
	// На Windows ln.Addr() для «любого интерфейса» может быть 0.0.0.0:port — тогда dial на 127.0.0.1 не попадает в слушатель.
	return stop, formatLocalProxyHTTPURL(ln.Addr()), nil
}

func acceptProxyLoop(ln net.Listener) {
	for {
		conn, accErr := ln.Accept()
		if accErr != nil {
			return
		}
		go handleProxyConn(conn)
	}
}

// formatLocalProxyHTTPURL даёт URL для клиента: всегда явный 127.0.0.1 вместо 0.0.0.0 / :: / пустого IP.
func formatLocalProxyHTTPURL(addr net.Addr) string {
	switch a := addr.(type) {
	case *net.TCPAddr:
		host := a.IP.String()
		if a.IP.IsUnspecified() || a.IP.Equal(net.IPv6loopback) {
			host = "127.0.0.1"
		}
		return "http://" + net.JoinHostPort(host, strconv.Itoa(a.Port))
	default:
		return "http://" + addr.String()
	}
}

func handleProxyConn(client net.Conn) {
	defer func() {
		_ = client.Close() //nolint:errcheck
	}()
	// Дедлайн на весь туннель: каталог и TLS могут быть дольше минуты.
	_ = client.SetDeadline(time.Now().Add(10 * time.Minute)) //nolint:errcheck

	br := bufio.NewReader(client)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Body != nil {
		_ = req.Body.Close() //nolint:errcheck
	}
	if req.Method != http.MethodConnect {
		// Для этого задания достаточно CONNECT (HTTPS через прокси).
		_, _ = fmt.Fprintf(client, "HTTP/1.1 501 Not Implemented\r\n\r\n") //nolint:errcheck
		return
	}
	host := req.Host
	if host == "" {
		return
	}
	dest, err := net.DialTimeout("tcp", host, 30*time.Second)
	if err != nil {
		_, _ = fmt.Fprintf(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n") //nolint:errcheck
		return
	}
	defer func() {
		_ = dest.Close() //nolint:errcheck
	}()
	_ = dest.SetDeadline(time.Now().Add(10 * time.Minute))                    //nolint:errcheck
	_, _ = fmt.Fprintf(client, "HTTP/1.1 200 Connection established\r\n\r\n") //nolint:errcheck

	// Два направления параллельно. Нельзя закрывать dest в одной горутине по EOF br —
	// иначе второе направление (ответ TLS с апстрима) обрывается (wsarecv / connection aborted).
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(dest, br) //nolint:errcheck
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, dest) //nolint:errcheck
	}()
	wg.Wait()
}
