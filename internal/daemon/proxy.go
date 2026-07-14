package daemon

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
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type auditProxy struct {
	mu        sync.Mutex
	server    *http.Server
	listener  net.Listener
	upstreams []*url.URL
	next      uint64
	requests  atomic.Int64
}

func newAuditProxy(upstreams []string) (*auditProxy, error) {
	proxy := &auditProxy{}
	for _, raw := range upstreams {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" || parsed.Scheme != "http" {
			return nil, fmt.Errorf("upstream proxy must use an http URL")
		}
		proxy.upstreams = append(proxy.upstreams, parsed)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	proxy.listener = listener
	proxy.server = &http.Server{Handler: proxy, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = proxy.server.Serve(listener) }()
	return proxy, nil
}

func (p *auditProxy) matchesUpstreams(rawValues []string) bool {
	if p == nil || len(rawValues) != len(p.upstreams) {
		return false
	}
	for index, raw := range rawValues {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.String() != p.upstreams[index].String() {
			return false
		}
	}
	return true
}

func (p *auditProxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	p.requests.Add(1)
	if request.Method == http.MethodConnect {
		p.serveConnect(writer, request)
		return
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if upstream := p.nextUpstream(); upstream != nil {
		transport.Proxy = http.ProxyURL(upstream)
	} else {
		transport.Proxy = nil
	}
	request.RequestURI = ""
	response, err := transport.RoundTrip(request)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	copyHeaders(writer.Header(), response.Header)
	writer.WriteHeader(response.StatusCode)
	_, _ = io.Copy(writer, response.Body)
}

func (p *auditProxy) serveConnect(writer http.ResponseWriter, request *http.Request) {
	upstream := p.nextUpstream()
	var target net.Conn
	var err error
	if upstream == nil {
		target, err = net.DialTimeout("tcp", request.Host, 10*time.Second)
	} else {
		target, err = p.connectUpstream(upstream, request.Host)
	}
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadGateway)
		return
	}
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		target.Close()
		http.Error(writer, "hijacking unsupported", http.StatusInternalServerError)
		return
	}
	client, _, err := hijacker.Hijack()
	if err != nil {
		target.Close()
		return
	}
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	go proxyCopy(target, client)
	go proxyCopy(client, target)
}

func (p *auditProxy) connectUpstream(upstream *url.URL, target string) (net.Conn, error) {
	connection, err := net.DialTimeout("tcp", upstream.Host, 10*time.Second)
	if err != nil {
		return nil, err
	}
	authorization := ""
	if upstream.User != nil {
		password, _ := upstream.User.Password()
		authorization = "Proxy-Authorization: Basic " + basicAuth(upstream.User.Username(), password) + "\r\n"
	}
	if _, err := fmt.Fprintf(connection, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s\r\n", target, target, authorization); err != nil {
		connection.Close()
		return nil, err
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodConnect})
	if err != nil {
		connection.Close()
		return nil, err
	}
	_ = response.Body.Close()
	if response.StatusCode/100 != 2 {
		connection.Close()
		return nil, fmt.Errorf("upstream CONNECT failed: %s", response.Status)
	}
	return connection, nil
}

func (p *auditProxy) nextUpstream() *url.URL {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.upstreams) == 0 {
		return nil
	}
	index := atomic.AddUint64(&p.next, 1) - 1
	return p.upstreams[index%uint64(len(p.upstreams))]
}

func (p *auditProxy) Status() ProxyStatus {
	if p == nil || p.listener == nil {
		return ProxyStatus{}
	}
	return ProxyStatus{Enabled: true, Listen: p.listener.Addr().String(), UpstreamCount: len(p.upstreams), RequestCount: p.requests.Load()}
}

func (p *auditProxy) Close() {
	if p != nil && p.server != nil {
		_ = p.server.Shutdown(context.Background())
	}
}

func (s *Server) executionCommand(command string, execution ExecutionEnvironment) (string, error) {
	environment, err := s.executionValues(execution)
	if err != nil {
		return "", err
	}
	if len(environment) == 0 {
		return command, nil
	}
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var prefix strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&prefix, "export %s=%s; ", key, shellQuote(environment[key]))
	}
	return prefix.String() + command, nil
}

func (s *Server) executionValues(execution ExecutionEnvironment) (map[string]string, error) {
	if !execution.AuditProxy && !execution.Shim && execution.Dylib == "" {
		return map[string]string{}, nil
	}
	environment := map[string]string{}
	if execution.AuditProxy {
		s.mu.Lock()
		if s.proxy == nil {
			proxy, err := newAuditProxy(execution.Upstreams)
			if err != nil {
				s.mu.Unlock()
				return nil, err
			}
			s.proxy = proxy
		} else if !s.proxy.matchesUpstreams(execution.Upstreams) {
			s.mu.Unlock()
			return nil, fmt.Errorf("audit proxy is already active with a different upstream pool")
		}
		status := s.proxy.Status()
		s.mu.Unlock()
		proxyURL := "http://" + status.Listen
		for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy"} {
			environment[key] = proxyURL
		}
		if len(execution.Bypass) > 0 {
			environment["NO_PROXY"] = strings.Join(execution.Bypass, ",")
			environment["no_proxy"] = environment["NO_PROXY"]
		}
		environment["NODE_USE_ENV_PROXY"] = "1"
	}
	if execution.Shim {
		dir, err := s.materializeShims()
		if err != nil {
			return nil, err
		}
		environment["PATH"] = dir + string(os.PathListSeparator) + os.Getenv("PATH")
		environment["BROWSER"] = filepath.Join(dir, "runtime-browser")
	}
	if execution.Dylib != "" {
		environment["DYLD_INSERT_LIBRARIES"] = execution.Dylib
	}
	return environment, nil
}

func (s *Server) materializeShims() (string, error) {
	dir := filepath.Join(s.config.Dir, "shims")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	content := []byte("#!/bin/sh\nexec sn-cli tools open-url \"$@\"\n")
	for _, name := range []string{"open", "xdg-open", "sensible-browser", "x-www-browser", "runtime-browser"} {
		if err := atomicWrite(filepath.Join(dir, name), content, 0o700); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func proxyCopy(destination io.WriteCloser, source io.ReadCloser) {
	_, _ = io.Copy(destination, source)
	_ = destination.Close()
	_ = source.Close()
}

func copyHeaders(destination, source http.Header) {
	for key, values := range source {
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

func basicAuth(username, password string) string {
	return base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
