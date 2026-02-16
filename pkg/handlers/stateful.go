package handlers

import (
	"bytes"
	"github.com/openfaas/faas-netes/pkg/k8s"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/openfaas/faas-provider/httputil"
	"github.com/openfaas/faas-provider/types"
)

const (
	watchdogPort       = "8080"
	defaultContentType = "text/plain"
)

// BaseURLResolver URL resolver for proxy requests
//
// The FaaS provider implementation is responsible for providing the resolver function implementation.
// BaseURLResolver.Resolve will receive the function name and should return the URL of the
// function service.
type BaseURLResolver interface {
	Resolve(functionName string) (url.URL, error)
}

// NewHandlerFunc creates a standard http.HandlerFunc to proxy function requests.
// The returned http.HandlerFunc will ensure:
//
//   - proper proxy request timeouts
//   - proxy requests for GET, POST, PATCH, PUT, and DELETE
//   - path parsing including support for extracing the function name, sub-paths, and query paremeters
//   - passing and setting the `X-Forwarded-Host` and `X-Forwarded-For` headers
//   - logging errors and proxy request timing to stdout
//
// Note that this will panic if `resolver` is nil.
func NewHandlerFunc(config types.FaaSConfig, resolver BaseURLResolver) http.HandlerFunc {
	if resolver == nil {
		panic("NewHandlerFunc: empty proxy handler resolver, cannot be nil")
	}

	proxyClient := NewProxyClientFromConfig(config)

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			defer r.Body.Close()
		}

		switch r.Method {
		case http.MethodPost,
			http.MethodPut,
			http.MethodPatch,
			http.MethodDelete,
			http.MethodGet,
			http.MethodOptions,
			http.MethodHead:
			proxyRequest(w, r, proxyClient, resolver)

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

// NewProxyClientFromConfig creates a new http.Client designed for proxying requests and enforcing
// certain minimum configuration values.
func NewProxyClientFromConfig(config types.FaaSConfig) *http.Client {
	return NewProxyClient(config.GetReadTimeout(), config.GetMaxIdleConns(), config.GetMaxIdleConnsPerHost())
}

// NewProxyClient creates a new http.Client designed for proxying requests, this is exposed as a
// convenience method for internal or advanced uses. Most people should use NewProxyClientFromConfig.
func NewProxyClient(timeout time.Duration, maxIdleConns int, maxIdleConnsPerHost int) *http.Client {
	return &http.Client{
		// these Transport values ensure that the http Client will eventually timeout and prevents
		// infinite retries. The default http.Client configure these timeouts.  The specific
		// values tuned via performance testing/benchmarking
		//
		// Additional context can be found at
		// - https://medium.com/@nate510/don-t-use-go-s-default-http-client-4804cb19f779
		// - https://blog.cloudflare.com/the-complete-guide-to-golang-net-http-timeouts/
		//
		// Additionally, these overrides for the default client enable re-use of connections and prevent
		// CoreDNS from rate limiting under high traffic
		//
		// See also two similar projects where this value was updated:
		// https://github.com/prometheus/prometheus/pull/3592
		// https://github.com/minio/minio/pull/5860
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   timeout,
				KeepAlive: 1 * time.Second,
				DualStack: true,
			}).DialContext,
			MaxIdleConns:          maxIdleConns,
			MaxIdleConnsPerHost:   maxIdleConnsPerHost,
			IdleConnTimeout:       120 * time.Millisecond,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1500 * time.Millisecond,
		},
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// proxyRequest handles the actual resolution of and then request to the function service.
func proxyRequest(w http.ResponseWriter, originalReq *http.Request, proxyClient *http.Client, resolver BaseURLResolver) {
	ctx := originalReq.Context()

	pathVars := mux.Vars(originalReq)
	functionName := pathVars["name"]
	if functionName == "" {
		httputil.Errorf(w, http.StatusBadRequest, "Provide function name in the request path")
		return
	}

	functionAddr, resolveErr := resolver.Resolve(functionName)
	if resolveErr != nil {
		// TODO: Should record the 404/not found error in Prometheus.
		log.Printf("resolver error: no endpoints for %s: %s\n", functionName, resolveErr.Error())
		httputil.Errorf(w, http.StatusServiceUnavailable, "No endpoints available for: %s.", functionName)
		return
	}

	// ===== 新增：读取原始 Body 并保存 =====
	var reqBodyBytes []byte
	var reqBodyErr error
	// 读取 originalReq.Body 的内容（仅读取一次，保存为字节数组）
	if originalReq.Body != nil {
		reqBodyBytes, reqBodyErr = io.ReadAll(originalReq.Body)
		if reqBodyErr != nil {
			httputil.Errorf(w, http.StatusInternalServerError, "Failed to read request body: %s", functionName)
			return
		}
		// 恢复 originalReq.Body（避免后续使用时为空）
		originalReq.Body = io.NopCloser(bytes.NewBuffer(reqBodyBytes))
	}

	proxyReq, err := buildProxyRequest(originalReq, functionAddr, pathVars["params"])
	if err != nil {
		httputil.Errorf(w, http.StatusInternalServerError, "Failed to resolve service: %s.", functionName)
		return
	}

	if proxyReq.Body != nil {
		defer proxyReq.Body.Close()
	}

	start := time.Now()
	//response, err := proxyClient.Do(proxyReq.WithContext(ctx))
	// 处理429状态码：需要重试
	var response *http.Response
	maxTime := 100
	for i := 0; i < maxTime; i++ {
		// 关键：每次重试前重置 Body 和 Content-Length
		if len(reqBodyBytes) > 0 {
			// 重新封装 Body（可多次读取）
			proxyReq.Body = io.NopCloser(bytes.NewBuffer(reqBodyBytes))
			// 恢复 Content-Length 头（关键！）
			proxyReq.ContentLength = int64(len(reqBodyBytes))
			proxyReq.Header.Set("Content-Length", strconv.Itoa(len(reqBodyBytes)))
		}
		response, err = proxyClient.Do(proxyReq.WithContext(ctx))
		log.Printf("response: %s, proxyReq: %s\n, err: %s", response, proxyReq, err)
		if response == nil {
			log.Printf("response is nil\n")
			time.Sleep(100 * time.Millisecond)
			//新增更新时重置，避免实例更新后请求失败
			functionAddr, resolveErr = resolver.Resolve(functionName)
			if resolveErr != nil {
				// TODO: Should record the 404/not found error in Prometheus.
				log.Printf("resolver error: no endpoints for %s: %s\n", functionName, resolveErr.Error())
				httputil.Errorf(w, http.StatusServiceUnavailable, "No endpoints available for: %s.", functionName)
				return
			}
			proxyReq, err = buildProxyRequest(originalReq, functionAddr, pathVars["params"])
			if err != nil {
				httputil.Errorf(w, http.StatusInternalServerError, "Failed to resolve service: %s.", functionName)
				return
			}
			if proxyReq.Body != nil {
				proxyReq.Body.Close()
			}

			continue
		} else if response.StatusCode == http.StatusTooManyRequests {
			log.Printf("function: %s too many requests\n", functionName)
			time.Sleep(100 * time.Millisecond)
			if response.Body != nil {
				_ = response.Body.Close()
			}
			continue
		} else {
			break
		}
	}

	seconds := time.Since(start)

	if err != nil {
		log.Printf("error with proxy request to: %s, %s\n", proxyReq.URL.String(), err.Error())

		httputil.Errorf(w, http.StatusInternalServerError, "Can't reach service for: %s.", functionName)
		return
	}

	if response.Body != nil {
		defer response.Body.Close()
	}

	// 5. 重置状态为Idle（无论成功/失败）
	namespace := "openfaas-fn"
	podIP := strings.Split(functionAddr.Host, ":")[0]
	k8s.GlobalInstanceManager.UpdateInstanceState(functionName, namespace, podIP, "idle")

	log.Printf("%s took %f seconds\n", functionName, seconds.Seconds())

	clientHeader := w.Header()
	copyHeaders(clientHeader, &response.Header)
	w.Header().Set("Content-Type", getContentType(originalReq.Header, response.Header))

	w.WriteHeader(response.StatusCode)
	if response.Body != nil {
		io.Copy(w, response.Body)
	}
}

// buildProxyRequest creates a request object for the proxy request, it will ensure that
// the original request headers are preserved as well as setting openfaas system headers
func buildProxyRequest(originalReq *http.Request, baseURL url.URL, extraPath string) (*http.Request, error) {

	host := baseURL.Host
	if baseURL.Port() == "" {
		host = baseURL.Host + ":" + watchdogPort
	}

	url := url.URL{
		Scheme:   baseURL.Scheme,
		Host:     host,
		Path:     extraPath,
		RawQuery: originalReq.URL.RawQuery,
	}

	upstreamReq, err := http.NewRequest(originalReq.Method, url.String(), nil)
	if err != nil {
		return nil, err
	}
	copyHeaders(upstreamReq.Header, &originalReq.Header)

	if len(originalReq.Host) > 0 && upstreamReq.Header.Get("X-Forwarded-Host") == "" {
		upstreamReq.Header["X-Forwarded-Host"] = []string{originalReq.Host}
	}
	if upstreamReq.Header.Get("X-Forwarded-For") == "" {
		upstreamReq.Header["X-Forwarded-For"] = []string{originalReq.RemoteAddr}
	}

	if originalReq.Body != nil {
		upstreamReq.Body = originalReq.Body
	}

	return upstreamReq, nil
}

// copyHeaders clones the header values from the source into the destination.
func copyHeaders(destination http.Header, source *http.Header) {
	for k, v := range *source {
		vClone := make([]string, len(v))
		copy(vClone, v)
		destination[k] = vClone
	}
}

// getContentType resolves the correct Content-Type for a proxied function.
func getContentType(request http.Header, proxyResponse http.Header) (headerContentType string) {
	responseHeader := proxyResponse.Get("Content-Type")
	requestHeader := request.Get("Content-Type")

	if len(responseHeader) > 0 {
		headerContentType = responseHeader
	} else if len(requestHeader) > 0 {
		headerContentType = requestHeader
	} else {
		headerContentType = defaultContentType
	}

	return headerContentType
}
