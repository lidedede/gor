package main

import (
	"io"
	"log"
	"sync/atomic"
	"time"

	"github.com/buger/goreplay/proto"
	"github.com/buger/goreplay/metrics"
)

const initialDynamicWorkers = 10

type response struct {
	payload       []byte
	uuid          []byte
	roundTripTime int64
	startedAt     int64
}

// HTTPOutputConfig struct for holding http output configuration
type HTTPOutputConfig struct {
	redirectLimit int

	stats      bool
	workersMin int
	workersMax int
	statsMs    int
	workers    int
	queueLen   int

	elasticSearch string

	Timeout      time.Duration
	OriginalHost bool
	BufferSize   int

	CompatibilityMode bool

	Debug bool

	TrackResponses bool
}

// HTTPOutput plugin manage pool of workers which send request to replayed server
// By default workers pool is dynamic and starts with 10 workers
// You can specify fixed number of workers using `--output-http-workers`
type HTTPOutput struct {
	// Keep this as first element of struct because it guarantees 64bit
	// alignment. atomic.* functions crash on 32bit machines if operand is not
	// aligned at 64bit. See https://github.com/golang/go/issues/599
	activeWorkers int64

	address string
	limit   int
	queue   chan []byte

	responses chan response

	needWorker chan int

	config *HTTPOutputConfig

	queueStats *GorStat

	elasticSearch *ESPlugin
}

// NewHTTPOutput constructor for HTTPOutput
// Initialize workers
func NewHTTPOutput(address string, config *HTTPOutputConfig) io.Writer {
	o := new(HTTPOutput)

	o.address = address
	o.config = config

	if o.config.stats {
		o.queueStats = NewGorStat("output_http", o.config.statsMs)
	}

	o.queue = make(chan []byte, o.config.queueLen)
	o.responses = make(chan response, o.config.queueLen)
	o.needWorker = make(chan int, 1)

	// Initial workers count
	if o.config.workersMax == 0 {
		o.needWorker <- initialDynamicWorkers
	} else {
		o.needWorker <- o.config.workersMax
	}

	if o.config.elasticSearch != "" {
		o.elasticSearch = new(ESPlugin)
		o.elasticSearch.Init(o.config.elasticSearch)
	}

	go o.workerMaster()

	return o
}

func (o *HTTPOutput) workerMaster() {
	for {
		newWorkers := <-o.needWorker
		for i := 0; i < newWorkers; i++ {
			go o.startWorker()
		}
	}
}

func (o *HTTPOutput) startWorker() {
	client := NewHTTPClient(o.address, &HTTPClientConfig{
		FollowRedirects:    o.config.redirectLimit,
		Debug:              o.config.Debug,
		OriginalHost:       o.config.OriginalHost,
		Timeout:            o.config.Timeout,
		ResponseBufferSize: o.config.BufferSize,
		CompatibilityMode:  o.config.CompatibilityMode,
	})

	deathCount := 0

	atomic.AddInt64(&o.activeWorkers, 1)

	for {
		select {
		case data := <-o.queue:
			o.sendRequest(client, data)
			deathCount = 0
		case <-time.After(time.Millisecond * 100):
			// When dynamic scaling enabled workers die after 2s of inactivity
			if o.config.workersMin == o.config.workersMax {
				continue
			}

			deathCount++
			if deathCount > 20 {
				workersCount := int(atomic.LoadInt64(&o.activeWorkers))

				// At least 1 startWorker should be alive
				if workersCount != 1 && workersCount > o.config.workersMin {
					atomic.AddInt64(&o.activeWorkers, -1)
					return
				}
			}
		}
	}
}

func (o *HTTPOutput) Write(data []byte) (n int, err error) {
	if !isRequestPayload(data) {
		return len(data), nil
	}

	buf := make([]byte, len(data))
	copy(buf, data)

	o.queue <- buf

	if o.config.stats {
		o.queueStats.Write(len(o.queue))
	}

	if o.config.workersMax != o.config.workersMin {
		workersCount := int(atomic.LoadInt64(&o.activeWorkers))

		if len(o.queue) > workersCount {
			extraWorkersReq := len(o.queue) - workersCount + 1
			maxWorkersAvailable := o.config.workersMax - workersCount
			if extraWorkersReq > maxWorkersAvailable {
				extraWorkersReq = maxWorkersAvailable
			}
			if extraWorkersReq > 0 {
				o.needWorker <- extraWorkersReq
			}
		}
	}

	return len(data), nil
}

func (o *HTTPOutput) Read(data []byte) (int, error) {
	resp := <-o.responses

	if Settings.debug {
		Debug("[OUTPUT-HTTP] Received response:", string(resp.payload))
	}

	header := payloadHeader(ReplayedResponsePayload, resp.uuid, resp.roundTripTime, resp.startedAt)
	copy(data[0:len(header)], header)
	copy(data[len(header):], resp.payload)

	return len(resp.payload) + len(header), nil
}

func (o *HTTPOutput) sendRequest(client *HTTPClient, request []byte) {
	meta := payloadMeta(request)

	if Settings.debug {
		Debug(meta)
	}

	if len(meta) < 2 {
		return
	}
	uuid := meta[1]

	body := payloadBody(request)
	if !proto.IsHTTPPayload(body) {
		return
	}

	start := time.Now()
	resp, err := client.Send(body)
	stop := time.Now()
	tc := time.Since(start)
	metrics.ObserveTotalRequestsTimeHistogram(req.RequestURI, tc.Seconds())
	metrics.IncreaseTotalRequests(req.RequestURI, string(resp.StatusCode))
	if err != nil {
		log.Println("Error when sending ", err, time.Now())
		Debug("Request error:", err)
	}

	if o.config.TrackResponses {
		o.responses <- response{resp, uuid, start.UnixNano(), stop.UnixNano() - start.UnixNano()}
	}

	if o.elasticSearch != nil {
		o.elasticSearch.ResponseAnalyze(request, resp, start, stop)
	}
}

func (o *HTTPOutput) String() string {
	return "HTTP output: " + o.address
}
