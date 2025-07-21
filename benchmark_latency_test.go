package muxado

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/benchmark/latency"
)

// TestScenario defines a benchmark test scenario
type TestScenario struct {
	PayloadSize int64
	Concurrency int
}

// TransportType defines a transport configuration for benchmarking
type TransportType struct {
	Name            string
	CreateFn        func(*latency.Network) (net.Conn, net.Conn)
	SupportsLatency bool
}

// Define transport types including memory transport with and without latency
var transportTypes = []TransportType{
	// {"TCP", tcpLatencyTransport, true},          // Commented out - already benchmarked
	// {"TLS", tlsLatencyTransport, true},          // Commented out - focus on memory transports
	{"Memory", memLatencyTransport, true},
	{"MemoryRaw", memRawTransport, false}, // Memory without latency wrapper
	{"SSH", sshLatencyTransport, true},    // SSH with latency injection
}

// Define latency profiles using gRPC's predefined networks plus custom profiles
var latencyProfiles = []struct {
	Name    string
	Network *latency.Network
}{
	{"Baseline", &latency.Network{Latency: 0, Kbps: 0, MTU: 0}},
	{"LAN", &latency.Network{Latency: 10 * time.Millisecond, Kbps: 0, MTU: 0}},
	{"Regional", &latency.Network{Latency: 50 * time.Millisecond, Kbps: 0, MTU: 0}},
	{"Internet", &latency.Network{Latency: 100 * time.Millisecond, Kbps: 0, MTU: 0}},
	// {"Intercontinental", &latency.Network{Latency: 200 * time.Millisecond, Kbps: 0, MTU: 0}}, // Commented out - focus on up to 100ms latency
	// {"Satellite", &latency.Network{Latency: 500 * time.Millisecond, Kbps: 0, MTU: 0}},        // Commented out - focus on up to 100ms latency  
	// {"Poor", &latency.Network{Latency: 1000 * time.Millisecond, Kbps: 0, MTU: 0}},           // Commented out - focus on up to 100ms latency
}

// Define test scenarios
var testScenarios = []TestScenario{
	// Core payload sizes with single stream
	{1024, 1},             // 1KB
	{1024 * 1024, 1},      // 1MB
	{16 * 1024 * 1024, 1}, // 16MB for debug logging test
	{32 * 1024 * 1024, 1}, // 32MB

	// Concurrency tests with 1KB payload
	{1024, 4},  // 4 streams
	{1024, 8},  // 8 streams
	{1024, 16}, // 16 streams
}

// LatencyMetrics collects performance and latency statistics
type LatencyMetrics struct {
	PayloadSize         int64
	Concurrency         int
	LatencyProfile      string
	Iterations          int
	TotalDuration       time.Duration
	TimePerOperation    time.Duration
	OperationsPerSec    float64
	ThroughputMBs       float64
	RelativePerf        float64
	NetworkLatency      time.Duration
	ConnectionSetupTime time.Duration
}

// TCP transport with latency injection using proper gRPC latency pattern
func tcpLatencyTransport(network *latency.Network) (net.Conn, net.Conn) {
	// Create a real TCP listener with latency injection
	baseListener, port := listener()
	latencyListener := network.Listener(baseListener)
	defer latencyListener.Close()

	// Use channel to coordinate connection establishment
	c := make(chan net.Conn)
	s := make(chan net.Conn)
	
	// Server side - accept connection through latency-wrapped listener
	go func() {
		conn, err := latencyListener.Accept()
		if err != nil {
			panic(err)
		}
		s <- conn
	}()
	
	// Client side - dial through latency-wrapped dialer
	go func() {
		dialer := network.Dialer(net.Dial)
		conn, err := dialer("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			panic(err)
		}
		c <- conn
	}()
	
	return <-c, <-s
}

// TLS transport with latency injection using proper gRPC latency pattern  
func tlsLatencyTransport(network *latency.Network) (net.Conn, net.Conn) {
	// First create the latency-injected TCP connections
	c, s := tcpLatencyTransport(network)

	// Initialize the global CA if needed
	initGlobalCA()
	
	// Set up client TLS config with the global CA
	roots := x509.NewCertPool()
	roots.AddCert(globalCA)
	clientTLSConf := &tls.Config{RootCAs: roots, ServerName: "snakeoil.dev"}
	
	// Generate server certificate signed by the global CA
	serverCert, _, err := genCert("snakeoil.dev", globalCA)
	if err != nil {
		panic(err)
	}
	
	return tls.Client(c, clientTLSConf), tls.Server(s, &tls.Config{Certificates: []tls.Certificate{*serverCert}})
}

// Memory transport with latency injection - simulate using localhost TCP for proper latency injection
func memLatencyTransport(network *latency.Network) (net.Conn, net.Conn) {
	// For memory transport with latency, we use localhost TCP connections
	// This provides proper network-like behavior for latency injection
	return tcpLatencyTransport(network)
}

// Memory transport without latency wrapper (raw performance)
func memRawTransport(network *latency.Network) (net.Conn, net.Conn) {
	// Ignore network parameter and return raw memory transport
	c, s := memTransport()
	return &rwcConn{c}, &rwcConn{s}
}

// SSH transport with latency injection
func sshLatencyTransport(network *latency.Network) (net.Conn, net.Conn) {
	// Use latency-injected TCP connections for SSH
	return tcpLatencyTransport(network)
}

// Helper function to format byte sizes for benchmark names
func formatSize(bytes int64) string {
	switch {
	case bytes >= 1024*1024*1024:
		return fmt.Sprintf("%dGB", bytes/(1024*1024*1024))
	case bytes >= 1024*1024:
		return fmt.Sprintf("%dMB", bytes/(1024*1024))
	case bytes >= 1024:
		return fmt.Sprintf("%dKB", bytes/1024)
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}

// Main latency benchmark function using sub-benchmarks with transport matrix
func BenchmarkLatency(b *testing.B) {
	for _, transport := range transportTypes {
		b.Run(transport.Name, func(b *testing.B) {
			// Create per-benchmark baseline metrics map for thread safety
			baselineMetrics := make(map[string]LatencyMetrics)

			for _, scenario := range testScenarios {
				scenarioName := fmt.Sprintf("Payload%s_Streams%d",
					formatSize(scenario.PayloadSize), scenario.Concurrency)

				b.Run(scenarioName, func(b *testing.B) {
					if transport.SupportsLatency {
						// Run all latency profiles for latency-capable transports
						for _, profile := range latencyProfiles {
							b.Run(profile.Name, func(b *testing.B) {
								testCaseWithTransportLatency(b, transport, scenario.PayloadSize,
									scenario.Concurrency, profile.Network, profile.Name, baselineMetrics)
							})
						}
					} else {
						// Run single baseline test for non-latency transports
						b.Run("Baseline", func(b *testing.B) {
							testCaseWithTransportLatency(b, transport, scenario.PayloadSize,
								scenario.Concurrency, nil, "Baseline", baselineMetrics)
						})
					}
				})
			}
		})
	}
}

// Core latency test function with transport support
func testCaseWithTransportLatency(b *testing.B, transport TransportType, payloadSize int64, concurrency int, network *latency.Network, profileName string, baselineMetrics map[string]LatencyMetrics) {
	done := make(chan int)

	// Use transport's create function - this happens before timing
	c, s := transport.CreateFn(network)

	// Choose session factory based on transport type
	var sessFactory func(io.ReadWriteCloser, bool) muxSession
	if transport.Name == "SSH" {
		sessFactory = newSSHAdaptor
	} else {
		sessFactory = newMuxadoAdaptor
	}
	
	go func() {
		latencyServer(b, sessFactory(s, true), payloadSize, concurrency, done, profileName, network, transport.Name, baselineMetrics)
	}()
	go latencyClient(b, sessFactory(c, false), payloadSize, profileName)
	<-done
}

func latencyServer(b *testing.B, sess muxSession, payloadSize int64, concurrency int, done chan int, profileName string, network *latency.Network, transportName string, baselineMetrics map[string]LatencyMetrics) {
	go wait(b, sess, "server")

	p := new(alot)

	// Start timing after connection and session setup
	b.ResetTimer()
	start := time.Now()

	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
		wg.Add(concurrency)
		startSignal := make(chan int)

		for c := 0; c < concurrency; c++ {
			go func() {
				<-startSignal
				str, err := sess.OpenStream()
				if err != nil {
					panic(err)
				}
				go func() {
					_, err := io.CopyN(ioutil.Discard, str, payloadSize)
					if err != nil {
						panic(err)
					}
					wg.Done()
					str.Close()
				}()
				n, err := io.CopyN(str, p, payloadSize)
				if n != payloadSize {
					b.Errorf("Server failed to send full payload. Got %d, expected %d", n, payloadSize)
				}
				if err != nil {
					panic(err)
				}
			}()
		}
		close(startSignal)
		wg.Wait()
	}

	// Collect metrics
	totalDuration := time.Since(start)
	metrics := calculateLatencyMetrics(b, payloadSize, concurrency, profileName, network, totalDuration, transportName, baselineMetrics)
	reportLatencyMetrics(b, metrics)

	close(done)
}

func latencyClient(b *testing.B, sess muxSession, expectedSize int64, profileName string) {
	go wait(b, sess, "client")

	for {
		str, err := sess.AcceptStream()
		if err != nil {
			panic(err)
		}

		go func(s muxStream) {
			n, err := io.CopyN(s, s, expectedSize)
			if err != nil {
				panic(err)
			}
			s.Close()
			if n != expectedSize {
				b.Errorf("stream with wrong size: %d, expected %d", n, expectedSize)
			}
		}(str)
	}
}

func calculateLatencyMetrics(b *testing.B, payloadSize int64, concurrency int, profileName string, network *latency.Network, totalDuration time.Duration, transportName string, baselineMetrics map[string]LatencyMetrics) LatencyMetrics {
	totalOps := int64(b.N * concurrency)
	totalBytes := totalOps * payloadSize

	timePerOp := totalDuration / time.Duration(b.N)
	opsPerSec := float64(totalOps) / totalDuration.Seconds()
	throughputMBs := float64(totalBytes) / totalDuration.Seconds() / (1024 * 1024)

	networkLatency := time.Duration(0)
	if network != nil {
		networkLatency = network.Latency
	}

	metrics := LatencyMetrics{
		PayloadSize:         payloadSize,
		Concurrency:         concurrency,
		LatencyProfile:      profileName,
		Iterations:          b.N,
		TotalDuration:       totalDuration,
		TimePerOperation:    timePerOp,
		OperationsPerSec:    opsPerSec,
		ThroughputMBs:       throughputMBs,
		NetworkLatency:      networkLatency,
		ConnectionSetupTime: 0, // Will be enhanced in future iteration
		RelativePerf:        1.0, // Will be calculated below
	}

	// Calculate relative performance against baseline
	baselineKey := fmt.Sprintf("%s_%d_%d", transportName, payloadSize, concurrency)
	if profileName == "Baseline" {
		// Store baseline for comparison
		baselineMetrics[baselineKey] = metrics
		metrics.RelativePerf = 1.0
	} else {
		// Compare against baseline
		if baseline, exists := baselineMetrics[baselineKey]; exists {
			metrics.RelativePerf = metrics.ThroughputMBs / baseline.ThroughputMBs
		}
	}

	return metrics
}

func reportLatencyMetrics(b *testing.B, metrics LatencyMetrics) {
	// Report in format: Latency | Iterations | Time/Operation | Ops/Second | Throughput (MB/s) | Relative Performance To Baseline | Network Latency
	fmt.Printf("LATENCY_METRICS: %s | %d | %v | %.2f | %.2f | %.3f | %v\n",
		metrics.LatencyProfile,
		metrics.Iterations,
		metrics.TimePerOperation,
		metrics.OperationsPerSec,
		metrics.ThroughputMBs,
		metrics.RelativePerf,
		metrics.NetworkLatency,
	)
}
