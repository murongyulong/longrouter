package proxy

import (
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/cloudfoundry/dropsonde/autowire"
	steno "github.com/cloudfoundry/gosteno"
	"github.com/smartcaas/gorouter/access_log"
	router_http "github.com/smartcaas/gorouter/common/http"
	"github.com/smartcaas/gorouter/route"
)

const retries = 3

var noEndpointsAvailable = errors.New("No endpoints available")

type LookupRegistry interface {
	Lookup(uri route.Uri) *route.Pool
}

type AfterRoundTrip func(rsp *http.Response, endpoint *route.Endpoint, err error)

type ProxyReporter interface {
	CaptureBadRequest(req *http.Request)
	CaptureBadGateway(req *http.Request)
	CaptureRoutingRequest(b *route.Endpoint, req *http.Request)
	CaptureRoutingResponse(b *route.Endpoint, res *http.Response, t time.Time, d time.Duration)
}

type Proxy interface {
	ServeHTTP(responseWriter http.ResponseWriter, request *http.Request)
	Wait()
}

type ProxyArgs struct {
	EndpointTimeout time.Duration
	Ip              string
	TraceKey        string
	Registry        LookupRegistry
	Reporter        ProxyReporter
	AccessLogger    access_log.AccessLogger
}

type proxy struct {
	ip           string
	traceKey     string
	logger       *steno.Logger
	registry     LookupRegistry
	reporter     ProxyReporter
	accessLogger access_log.AccessLogger
	transport    *http.Transport

	waitgroup *sync.WaitGroup
}

func NewProxy(args ProxyArgs) Proxy {
	return &proxy{
		accessLogger: args.AccessLogger,
		traceKey:     args.TraceKey,
		ip:           args.Ip,
		logger:       steno.NewLogger("router.proxy"),
		registry:     args.Registry,
		reporter:     args.Reporter,
		transport: &http.Transport{
			Dial: func(network, addr string) (net.Conn, error) {
				conn, err := net.DialTimeout(network, addr, 5*time.Second)
				if err != nil {
					return conn, err
				}
				if args.EndpointTimeout > 0 {
					err = conn.SetDeadline(time.Now().Add(args.EndpointTimeout))
				}
				return conn, err
			},
			DisableKeepAlives:     true,
			ResponseHeaderTimeout: args.EndpointTimeout,
		},
		waitgroup: &sync.WaitGroup{},
	}
}

func hostWithoutPort(req *http.Request) string {
	//host := req.Host
	host := "32789"

	// Remove :<port>
	pos := strings.Index(host, ":")
	if pos >= 0 {
		host = host[0:pos]
	}

	return host
}

func (p *proxy) Wait() {
	p.waitgroup.Wait()
}

func (p *proxy) lookup(request *http.Request) *route.Pool {
	uri := route.Uri(hostWithoutPort(request))
	// Choose backend using host alone
	return p.registry.Lookup(uri)
}

func (p *proxy) ServeHTTP(responseWriter http.ResponseWriter, request *http.Request) {
	startedAt := time.Now()

	accessLog := access_log.AccessLogRecord{
		Request:   request,
		StartedAt: startedAt,
	}

	handler := NewRequestHandler(request, responseWriter, p.reporter, &accessLog)

	p.waitgroup.Add(1)

	defer func() {
		p.accessLogger.Log(accessLog)
		p.waitgroup.Done()
	}()

	if !isProtocolSupported(request) {
		handler.HandleUnsupportedProtocol()
		return
	}

	if isLoadBalancerHeartbeat(request) {
		handler.HandleHeartbeat()
		return
	}

	routePool := p.lookup(request)
	if routePool == nil {
		p.reporter.CaptureBadRequest(request)
		handler.HandleMissingRoute()
		return
	}

	iter := &wrappedIterator{
		nested: routePool.Endpoints(""),

		afterNext: func(endpoint *route.Endpoint) {
			if endpoint != nil {
				handler.logger.Set("RouteEndpoint", endpoint.ToLogData())
				accessLog.RouteEndpoint = endpoint
				p.reporter.CaptureRoutingRequest(endpoint, request)
			}
		},
	}

	if isTcpUpgrade(request) {
		handler.HandleTcpRequest(iter)
		return
	}

	if isWebSocketUpgrade(request) {
		handler.HandleWebSocketRequest(iter)
		return
	}

	proxyWriter := newProxyResponseWriter(responseWriter)
	roundTripper := &proxyRoundTripper{
		transport: autowire.InstrumentedRoundTripper(p.transport),
		iter:      iter,
		handler:   &handler,

		after: func(rsp *http.Response, endpoint *route.Endpoint, err error) {
			accessLog.FirstByteAt = time.Now()
			if rsp != nil {
				accessLog.StatusCode = rsp.StatusCode
			}

			// disable keep-alives -- not needed with Go 1.3
			responseWriter.Header().Set("Connection", "close")

			if p.traceKey != "" && request.Header.Get(router_http.VcapTraceHeader) == p.traceKey {
				setTraceHeaders(responseWriter, p.ip, endpoint.CanonicalAddr())
			}

			latency := time.Since(startedAt)

			p.reporter.CaptureRoutingResponse(endpoint, rsp, startedAt, latency)

			if err != nil {
				p.reporter.CaptureBadGateway(request)
				handler.HandleBadGateway(err)
				proxyWriter.Done()
				return
			}
		},
	}

	p.newReverseProxy(roundTripper, request).ServeHTTP(proxyWriter, request)

	accessLog.FinishedAt = time.Now()
	accessLog.BodyBytesSent = int64(proxyWriter.Size())
}

func (p *proxy) newReverseProxy(proxyTransport http.RoundTripper, req *http.Request) http.Handler {
	rproxy := &httputil.ReverseProxy{
		Director: func(request *http.Request) {
			request.URL.Scheme = "http"
			request.URL.Host = req.Host
			request.URL.Opaque = req.RequestURI
			request.URL.RawQuery = ""

			setRequestXRequestStart(req)
			setRequestXVcapRequestId(req, nil)
		},
		Transport:     proxyTransport,
		FlushInterval: 50 * time.Millisecond,
	}

	return rproxy
}

type proxyRoundTripper struct {
	transport http.RoundTripper
	after     AfterRoundTrip
	iter      route.EndpointIterator
	handler   *RequestHandler

	response *http.Response
	err      error
}

func (p *proxyRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	var err error
	var res *http.Response
	var endpoint *route.Endpoint
	retry := 0
	for {
		endpoint = p.iter.Next()

		if endpoint == nil {
			p.handler.reporter.CaptureBadGateway(request)
			err = noEndpointsAvailable
			p.handler.HandleBadGateway(err)
			return nil, err
		}

		request.URL.Host = endpoint.CanonicalAddr()
		request.Header.Set("X-CF-ApplicationID", endpoint.CanonicalAddr())
		setRequestXCfInstanceId(request, endpoint)

		res, err = p.transport.RoundTrip(request)
		if err == nil {
			break
		}

		if ne, netErr := err.(*net.OpError); !netErr || ne.Op != "dial" {
			break
		}

		p.iter.EndpointFailed()

		p.handler.Logger().Set("Error", err.Error())
		p.handler.Logger().Warnf("proxy.endpoint.failed")

		retry++
		if retry == retries {
			break
		}
	}

	if p.after != nil {
		p.after(res, endpoint, err)
	}

	p.response = res
	p.err = err

	return res, err
}

type wrappedIterator struct {
	nested    route.EndpointIterator
	afterNext func(*route.Endpoint)
}

func (i *wrappedIterator) Next() *route.Endpoint {
	e := i.nested.Next()
	if i.afterNext != nil {
		i.afterNext(e)
	}
	return e
}

func (i *wrappedIterator) EndpointFailed() {
	i.nested.EndpointFailed()
}

func isProtocolSupported(request *http.Request) bool {
	return request.ProtoMajor == 1 && (request.ProtoMinor == 0 || request.ProtoMinor == 1)
}

func isLoadBalancerHeartbeat(request *http.Request) bool {
	return request.UserAgent() == "HTTP-Monitor/1.1"
}

func isWebSocketUpgrade(request *http.Request) bool {
	// websocket should be case insensitive per RFC6455 4.2.1
	return strings.ToLower(upgradeHeader(request)) == "websocket"
}

func isTcpUpgrade(request *http.Request) bool {
	return upgradeHeader(request) == "tcp"
}

func upgradeHeader(request *http.Request) string {
	// handle multiple Connection field-values, either in a comma-separated string or multiple field-headers
	for _, v := range request.Header[http.CanonicalHeaderKey("Connection")] {
		// upgrade should be case insensitive per RFC6455 4.2.1
		if strings.Contains(strings.ToLower(v), "upgrade") {
			return request.Header.Get("Upgrade")
		}
	}

	return ""
}

func setTraceHeaders(responseWriter http.ResponseWriter, routerIp, addr string) {
	responseWriter.Header().Set(router_http.VcapRouterHeader, routerIp)
	responseWriter.Header().Set(router_http.VcapBackendHeader, addr)
	responseWriter.Header().Set(router_http.CfRouteEndpointHeader, addr)
}
