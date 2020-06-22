package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	totalRequestsCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "goreplay_total_requests",
			Help: "total income requests",
		},
		[]string{"location", "code"},
	)
	subRequestsCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "test_sub_requests",
			Help: "sub requests",
		},
		[]string{"test"},
	)
	circuitBreakerRateGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "goreplay_circuit_breaker_rate",
			Help: "rate of circuit breaker",
		},
		[]string{"location", "code"},
	)

	buckets = []float64{0, 100, 200}

	totalRequestsTimeHistogram = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "goreplay_total_requests_time",
			Help:    "income requests time",
			Buckets: buckets,
		},
		[]string{"location"},
	)

)

func init() {
	prometheus.MustRegister(totalRequestsCounter)
	prometheus.MustRegister(subRequestsCounter)
	prometheus.MustRegister(circuitBreakerRateGauge)
	prometheus.MustRegister(totalRequestsTimeHistogram)
}

func IncreaseTotalRequests(location,code string) {
	totalRequestsCounter.With(prometheus.Labels{"location": location, "code": code}).Add(1)
}

func IncreaseSubRequests() {
	subRequestsCounter.With(prometheus.Labels{}).Add(1)
}


func ObserveTotalRequestsTimeHistogram(location string, d float64) {
	totalRequestsTimeHistogram.With(prometheus.Labels{"location": location}).Observe(d)
}
