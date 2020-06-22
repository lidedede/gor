package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"github.com/buger/goreplay/proto"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/buger/goreplay/metrics"
)

var httpMu sync.Mutex

const (
	readChunkSize   = 64 * 1024
	maxResponseSize = 1073741824
)

var chunkedSuffix = []byte("0\r\n\r\n")

var defaultPorts = map[string]string{
	"http":  "80",
	"https": "443",
}

type HTTPClientConfig struct {
	FollowRedirects    int
	Debug              bool
	OriginalHost       bool
	ConnectionTimeout  time.Duration
	Timeout            time.Duration
	ResponseBufferSize int
	CompatibilityMode  bool
}

type HTTPClient struct {
	baseURL        string
	scheme         string
	host           string
	auth           string
	conn           net.Conn
	proxy          *url.URL
	proxyAuth      string
	respBuf        []byte
	config         *HTTPClientConfig
	goClient       *http.Client
	redirectsCount int
}

func NewHTTPClient(baseURL string, config *HTTPClientConfig) *HTTPClient {
	if !strings.HasPrefix(baseURL, "http") {
		baseURL = "http://" + baseURL
	}

	u, _ := url.Parse(baseURL)

	if config.Timeout == 0 {
		config.Timeout = time.Second
	}

	config.ConnectionTimeout = config.Timeout

	if config.ResponseBufferSize == 0 {
		config.ResponseBufferSize = 100 * 1024 // 100kb
	}

	client := new(HTTPClient)
	client.baseURL = u.String()
	client.host = u.Host
	client.scheme = u.Scheme
	client.respBuf = make([]byte, config.ResponseBufferSize)
	client.config = config

	if config.CompatibilityMode {
		client.goClient = &http.Client{
			// #TODO
			// CheckRedirect: redirectPolicyFunc,
		}
	}

	if u.User != nil {
		client.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(u.User.String()))
	}

	client.proxy, _ = http.ProxyFromEnvironment(&http.Request{URL: u})

	if client.isProxy() && client.proxy.User != nil {
		client.proxyAuth = "Basic " + base64.StdEncoding.EncodeToString([]byte(client.proxy.User.String()))
	}

	return client
}

func (c *HTTPClient) Connect() (err error) {
	c.Disconnect()

	var toDial string
	if !strings.Contains(c.host, ":") {
		toDial = c.host + ":" + defaultPorts[c.scheme]
	} else {
		toDial = c.host
	}

	if c.isProxy() {
		if c.proxy.Scheme != "http" {
			panic("Unsupported HTTP Proxy method")
		}
		Debug("[HTTPClient] Connecting to proxy", c.proxy.String(), "<>", toDial)
		c.conn, err = net.DialTimeout("tcp", c.proxy.Host, c.config.ConnectionTimeout)
		if err != nil {
			return
		}
		if c.scheme == "https" {
			c.conn.Write([]byte("CONNECT " + toDial + " HTTP/1.1\r\n"))
			if c.proxyAuth != "" {
				c.conn.Write([]byte("Proxy-Authorization: " + c.proxyAuth + "\r\n"))
			}
			c.conn.Write([]byte("\r\n"))
			br := bufio.NewReader(c.conn)
			l, _, err := br.ReadLine()
			if err != nil {
				return err
			}
			if len(l) < 12 {
				panic("HTTP proxy did not respond correctly")
			}
			status := l[9:12]
			if !bytes.Equal(status, []byte("200")) {
				panic("HTTP proxy did not respond correctly")
			}
			for {
				// Read until we find the empty line
				l, _, err := br.ReadLine()
				if err != nil {
					return err
				}
				if len(l) == 0 {
					break
				}
			}
		}
		Debug("[HTTPClient] Proxy successfully connected")
	} else {
		c.conn, err = net.DialTimeout("tcp", toDial, c.config.ConnectionTimeout)
		if err != nil {
			return
		}
	}

	if c.scheme == "https" {
		// Wrap our socket in TLS
		Debug("[HTTPClient] Wrapping socket in TLS", c.host)
		tlsConn := tls.Client(c.conn, &tls.Config{InsecureSkipVerify: true, ServerName: c.host})

		if err = tlsConn.Handshake(); err != nil {
			return
		}

		c.conn = tlsConn
		Debug("[HTTPClient] Successfully wrapped in TLS")
	}

	return
}

func (c *HTTPClient) Disconnect() {
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
		Debug("[HTTP] Disconnected: ", c.baseURL)
	}
}

func (c *HTTPClient) isAlive(readBytes *int) bool {
	// Ready 1 byte from socket without timeout to check if it not closed
	c.conn.SetReadDeadline(time.Now().Add(time.Millisecond))
	n, err := c.conn.Read(c.respBuf[:1])

	if err == io.EOF {
		Debug("[HTTPClient] connection closed, reconnecting")
		return false
	}

	if err == syscall.EPIPE {
		Debug("Detected broken pipe.", err)
		return false
	}
	if n != 0 {
		*readBytes += n
		Debug("[HTTPClient] isAlive readBytes ", *readBytes)
	}
	return true
}

func (c *HTTPClient) SendGoClient(data []byte) ([]byte, error) {
	var req *http.Request
	var resp *http.Response
	var err error

	req, err = http.ReadRequest(bufio.NewReader(bytes.NewBuffer(data)))
	if err != nil {
		return nil, err
	}

	if !c.config.OriginalHost {
		req.Host = c.host
	}

	if c.auth != "" {
		req.Header.Add("Authorization", c.auth)
	}

	req.URL, _ = url.ParseRequestURI(c.scheme + "://" + c.host + req.RequestURI)
	req.RequestURI = ""
	startT := time.Now()
	resp, err = c.goClient.Do(req)
	tc := time.Since(startT)
	if err != nil {
		return nil, err
	}
	metrics.ObserveTotalRequestsTimeHistogram(req.RequestURI, tc.Seconds())
	metrics.IncreaseTotalRequests(req.RequestURI, resp.Status)
	return httputil.DumpResponse(resp, true)
}

func (c *HTTPClient) Send(data []byte) (response []byte, err error) {
	// Don't exit on panic
	metrics.IncreaseSubRequests()
	defer func() {
		if r := recover(); r != nil {
			Debug("[HTTPClient]", r, string(data))

			if _, ok := r.(error); ok {
				log.Println("[HTTPClient] Failed to send request: ", string(data))
				log.Println("[HTTPClient] Response: ", string(response))
				log.Println("PANIC: pkg:", r, string(debug.Stack()))
			}
		}
	}()

	if c.config.CompatibilityMode {
		return c.SendGoClient(data)
	}

	var readBytes int
	if c.conn == nil || !c.isAlive(&readBytes) {
		Debug("[HTTPClient] Connecting:", c.baseURL)
		if err = c.Connect(); err != nil {
			log.Println("[HTTPClient] Connection error:", err)
			response = errorPayload(HTTP_CONNECTION_ERROR)
			return
		}
	}

	timeout := time.Now().Add(c.config.Timeout)

	c.conn.SetWriteDeadline(timeout)

	if !c.config.OriginalHost {
		data = proto.SetHost(data, []byte(c.baseURL), []byte(c.host))
	}

	if c.isProxy() && c.scheme == "http" {
		path := proto.Path(data)
		if len(path) > 0 && path[0] == '/' {
			data = proto.SetPath(data, c.proxyPath(path))
			if c.proxyAuth != "" {
				data = proto.SetHeader(data, []byte("Proxy-Authorization"), []byte(c.proxyAuth))
			}
		}
	}

	if c.auth != "" {
		data = proto.SetHeader(data, []byte("Authorization"), []byte(c.auth))
	}

	if c.config.Debug {
		Debug("[HTTPClient] Sending:", string(data))
	}
	return c.send(data, readBytes, timeout)
}

func (c *HTTPClient) send(data []byte, readBytes int, timeout time.Time) (response []byte, err error) {
	var payload []byte
	var n int
	if _, err = c.conn.Write(data); err != nil {
		Debug("[HTTPClient] Write error:", err, c.baseURL)
		response = errorPayload(HTTP_TIMEOUT)
		c.Disconnect()
		return
	}

	var currentChunk []byte
	timeout = time.Now().Add(c.config.Timeout)
	chunked := false
	contentLength := -1
	currentContentLength := 0
	chunks := 0

	for {
		c.conn.SetReadDeadline(timeout)

		if readBytes < len(c.respBuf) {
			n, err = c.conn.Read(c.respBuf[readBytes:])
			readBytes += n
			chunks++

			// First chunk
			if chunked || contentLength != -1 {
				currentContentLength += n
			} else {
				// If headers are finished
				var firstEmptyLine = bytes.Index(c.respBuf[:readBytes], proto.EmptyLine)
				if firstEmptyLine != -1 {
					if bytes.Equal(proto.Header(c.respBuf[:readBytes], []byte("Transfer-Encoding")), []byte("chunked")) {
						chunked = true
					} else {
						status, _ := strconv.Atoi(string(proto.Status(c.respBuf[:readBytes])))
						// We want to soak up all 100 Continues received to get the real result code
						if status >= 100 && status < 200 {
							timeout = time.Now().Add(c.config.Timeout)
							var deleteLen = firstEmptyLine + len(proto.EmptyLine)
							copy(c.respBuf, c.respBuf[deleteLen:readBytes])
							readBytes -= deleteLen
							chunks--
							continue
						} else if status == 204 || status == 304 {
							contentLength = 0
							break
						} else {
							l := proto.Header(c.respBuf[:readBytes], []byte("Content-Length"))
							if len(l) > 0 {
								contentLength, _ = strconv.Atoi(string(l))
							}
						}
					}

					currentContentLength += len(proto.Body(c.respBuf[:readBytes]))
				}
			}

			if chunked {
				// Check if chunked message finished
				if bytes.HasSuffix(c.respBuf[:readBytes], chunkedSuffix) {
					break
				}
			} else if contentLength != -1 {
				if currentContentLength > contentLength {
					Debug("[HTTPClient] disconnected, wrong length", currentContentLength, contentLength)
					c.Disconnect()
					break
				} else if currentContentLength == contentLength {
					break
				}
			}

			if err != nil {
				if err == io.EOF {
					err = nil
				}
				break
			}
		} else {
			if currentChunk == nil {
				currentChunk = make([]byte, readChunkSize)
			}

			n, err = c.conn.Read(currentChunk)

			readBytes += int(n)
			chunks++
			currentContentLength += n

			if chunked {
				// Check if chunked message finished
				if bytes.HasSuffix(currentChunk[:n], chunkedSuffix) {
					break
				}
			} else if contentLength != -1 {
				if currentContentLength > contentLength {
					Debug("[HTTPClient] disconnected, wrong length", currentContentLength, contentLength)
					c.Disconnect()
					break
				} else if currentContentLength == contentLength {
					break
				}
			} else {
				Debug("[HTTPClient] disconnected, can't find Content-Length or Chunked")
				c.Disconnect()
				break
			}

			if err == io.EOF {
				break
			} else if err != nil {
				Debug("[HTTPClient] Read the whole body error:", err, c.baseURL)
				break
			}

		}

		if readBytes >= maxResponseSize {
			Debug("[HTTPClient] Body is more than the max size", maxResponseSize,
				c.baseURL)
			break
		}

		// For following chunks expect less timeout
		timeout = time.Now().Add(c.config.Timeout / 5)
	}

	if err != nil && readBytes == 0 {
		maxRead := 100
		if readBytes < maxRead {
			maxRead = readBytes
		}
		Debug("[HTTPClient] Response read timeout error", err, c.conn, readBytes, string(c.respBuf[:maxRead]))
		response = errorPayload(HTTP_TIMEOUT)
		c.Disconnect()
		return
	}

	if readBytes < 4 || string(c.respBuf[:4]) != "HTTP" {
		maxRead := 100
		if readBytes < maxRead {
			maxRead = readBytes
		}
		Debug("[HTTPClient] Response read unknown error", err, c.conn, readBytes, string(c.respBuf[:maxRead]))
		response = errorPayload(HTTP_UNKNOWN_ERROR)
		c.Disconnect()
		return
	}

	if readBytes > len(c.respBuf) {
		readBytes = len(c.respBuf)
	}
	payload = make([]byte, readBytes)
	copy(payload, c.respBuf[:readBytes])

	if c.config.Debug {
		Debug("[HTTPClient] Received:", string(payload))
	}

	if c.config.FollowRedirects > 0 && c.redirectsCount < c.config.FollowRedirects {
		status := payload[9:12]

		// 3xx requests
		if status[0] == '3' {
			c.redirectsCount++

			location := proto.Header(payload, []byte("Location"))
			redirectPayload := proto.SetPath(data, location)

			if c.config.Debug {
				Debug("[HTTPClient] Redirecting to: " + string(location))
			}

			return c.Send(redirectPayload)
		}
	}

	if bytes.Equal(proto.Status(payload), []byte("400")) {
		Debug("[HTTPClient] Closed connection on 400 response")
		c.Disconnect()
	}

	c.redirectsCount = 0

	return payload, err
}

func (c *HTTPClient) Get(path string) (response []byte, err error) {
	payload := "GET " + path + " HTTP/1.1\r\n\r\n"

	return c.Send([]byte(payload))
}

func (c *HTTPClient) Post(path string, body []byte) (response []byte, err error) {
	payload := "POST " + path + " HTTP/1.1\r\n"
	payload += "Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n"
	payload += string(body)

	return c.Send([]byte(payload))
}

func (c *HTTPClient) proxyPath(path []byte) []byte {
	return append([]byte(c.scheme+"://"+c.host), path...)
}

func (c *HTTPClient) isProxy() bool {
	return c.proxy != nil
}

const (
	// https://support.cloudflare.com/hc/en-us/articles/200171936-Error-520-Web-server-is-returning-an-unknown-error
	HTTP_UNKNOWN_ERROR = "520"
	// https://support.cloudflare.com/hc/en-us/articles/200171916-Error-521-Web-server-is-down
	HTTP_CONNECTION_ERROR = "521"
	// https://support.cloudflare.com/hc/en-us/articles/200171906-Error-522-Connection-timed-out
	HTTP_CONNECTION_TIMEOUT = "522"
	// https://support.cloudflare.com/hc/en-us/articles/200171946-Error-523-Origin-is-unreachable
	HTTP_UNREACHABLE = "523"
	// https://support.cloudflare.com/hc/en-us/articles/200171926-Error-524-A-timeout-occurred
	HTTP_TIMEOUT = "524"
)

var errorPayloadTemplate = "HTTP/1.1 202 Accepted\r\nDate: Mon, 17 Aug 2015 14:10:11 GMT\r\nContent-Length: 0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n"

func errorPayload(errorCode string) []byte {
	payload := make([]byte, len(errorPayloadTemplate))
	copy(payload, errorPayloadTemplate)

	copy(payload[29:58], []byte(time.Now().Format(time.RFC1123)))
	copy(payload[9:12], errorCode)

	return payload
}
