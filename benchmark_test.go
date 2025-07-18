package muxado

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc/benchmark/latency"
)

type muxSession interface {
	OpenStream() (muxStream, error)
	AcceptStream() (muxStream, error)
	Wait() (error, error, []byte)
}

type muxStream interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	//CloseWrite() error
	Close() error
}

func BenchmarkPayload1BStreams1(b *testing.B) {
	testCase(b, 1, 1)
}

func BenchmarkPayload1KBStreams1(b *testing.B) {
	testCase(b, 1024, 1)
}

func BenchmarkPayload1MBStreams1(b *testing.B) {
	testCase(b, 1024*1024, 1)
}

func BenchmarkPayload32MBStreams1(b *testing.B) {
	testCase(b, 32*1024*1024, 1)
}

func BenchmarkPayload1BStreams4(b *testing.B) {
	testCase(b, 1, 4)
}

func BenchmarkPayload1KBStreams4(b *testing.B) {
	testCase(b, 1024, 4)
}

func BenchmarkPayload1MBStreams4(b *testing.B) {
	testCase(b, 1024*1024, 4)
}

func BenchmarkPayload32MBStreams4(b *testing.B) {
	testCase(b, 32*1024*1024, 4)
}

func BenchmarkPayload1BStreams8(b *testing.B) {
	testCase(b, 1, 8)
}

func BenchmarkPayload1KBStreams8(b *testing.B) {
	testCase(b, 1024, 8)
}

func BenchmarkPayload1MBStreams8(b *testing.B) {
	testCase(b, 1024*1024, 8)
}

func BenchmarkPayload32MBStreams8(b *testing.B) {
	testCase(b, 32*1024*1024, 8)
}

func BenchmarkPayload1BStreams64(b *testing.B) {
	testCase(b, 1, 64)
}

func BenchmarkPayload1KBStreams64(b *testing.B) {
	testCase(b, 1024, 64)
}

func BenchmarkPayload1MBStreams64(b *testing.B) {
	testCase(b, 1024*1024, 64)
}

func BenchmarkPayload32MBStreams64(b *testing.B) {
	testCase(b, 32*1024*1024, 64)
}

func BenchmarkPayload1BStreams256(b *testing.B) {
	testCase(b, 1, 256)
}

func BenchmarkPayload1KBStreams256(b *testing.B) {
	testCase(b, 1024, 256)
}

func BenchmarkPayload1MBStreams256(b *testing.B) {
	testCase(b, 1024*1024, 256)
}

func testCase(b *testing.B, payloadSize int64, concurrency int) {
	done := make(chan int)
	c, s := tlsTransport()
	sessFactory := newMuxadoAdaptor
	//sessFactory := newYamuxAdaptor
	//sessFactory := newSSHAdaptor
	go func() { server(b, sessFactory(s, true), payloadSize, concurrency, done) }()
	go client(b, sessFactory(c, false), payloadSize)
	<-done
}

func server(b *testing.B, sess muxSession, payloadSize int64, concurrency int, done chan int) {
	go wait(b, sess, "server")

	p := new(alot)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
		wg.Add(concurrency)
		start := make(chan int)
		for c := 0; c < concurrency; c++ {
			go func() {
				<-start
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
		close(start)
		wg.Wait()
	}
	close(done)
}

func client(b *testing.B, sess muxSession, expectedSize int64) {
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

func wait(b *testing.B, sess muxSession, name string) {
	localErr, remoteErr, _ := sess.Wait()
	localCode, _ := GetError(localErr)
	remoteCode, _ := GetError(remoteErr)
	fmt.Printf("'%s' session died with local err %v (code 0x%x), and remote err %v (code 0x%x)\n", name, localErr, localCode, remoteErr, remoteCode)
	if localCode != NoError || remoteCode != NoError {
		b.Errorf("bad session shutdown")
	}
}

var sourceBuf = bytes.Repeat([]byte("0123456789"), 12800)

type alot struct{}

func (a *alot) Read(p []byte) (int, error) {
	copy(p, sourceBuf)
	return len(p), nil
}

func tcpTransport() (net.Conn, net.Conn) {
	l, port := listener()
	defer l.Close()
	c := make(chan net.Conn)
	s := make(chan net.Conn)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			panic(err)
		}
		s <- conn
	}()
	go func() {
		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			panic(err)
		}
		c <- conn
	}()
	return <-c, <-s
}

func tlsTransport() (net.Conn, net.Conn) {
	c, s := tcpTransport()

	_, ca, err := genCert("Snakeoil CA", nil)
	if err != nil {
		panic(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)

	clientTLSConf := &tls.Config{RootCAs: roots}
	if err != nil {
		panic(err)
	}

	serverCert, _, err := genCert("snakeoil.dev", ca)
	if err != nil {
		panic(err)
	}
	return tls.Client(c, clientTLSConf), tls.Server(s, &tls.Config{Certificates: []tls.Certificate{*serverCert}})
}

// Global CA for TLS benchmarks to avoid regenerating
var (
	globalCA     *x509.Certificate
	globalCACert *tls.Certificate
	globalCAKey  *rsa.PrivateKey
)

func initGlobalCA() {
	if globalCA != nil {
		return
	}
	
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	globalCAKey = key
	
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		panic(err)
	}
	
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: "Snakeoil CA",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	
	certBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	
	x509Certs, err := x509.ParseCertificates(certBytes)
	if err != nil {
		panic(err)
	}
	
	globalCA = x509Certs[0]
	globalCACert = &tls.Certificate{
		Certificate: [][]byte{certBytes},
		PrivateKey:  key,
	}
}

func genCert(cn string, parent *x509.Certificate) (*tls.Certificate, *x509.Certificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, nil, err
	}
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: cn,
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment,
		BasicConstraintsValid: true,
		DNSNames:              []string{cn},
	}
	
	var signingKey *rsa.PrivateKey
	if parent == nil {
		// Self-signed certificate (CA)
		parent = &template
		template.KeyUsage |= x509.KeyUsageCertSign
		template.IsCA = true
		signingKey = key
	} else {
		// Certificate signed by parent CA
		signingKey = globalCAKey
	}
	
	certBytes, err := x509.CreateCertificate(rand.Reader, &template, parent, &key.PublicKey, signingKey)
	if err != nil {
		return nil, nil, err
	}
	x509Certs, err := x509.ParseCertificates(certBytes)
	if err != nil {
		return nil, nil, err
	}

	return &tls.Certificate{
		Certificate: [][]byte{certBytes},
		PrivateKey:  key,
	}, x509Certs[0], nil
}

func listener() (net.Listener, int) {
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		panic(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	return l, port
}

type duplexPipe struct {
	*io.PipeReader
	*io.PipeWriter
}

func (dp *duplexPipe) Close() error {
	dp.PipeReader.Close()
	dp.PipeWriter.Close()
	return nil
}

func memTransport() (io.ReadWriteCloser, io.ReadWriteCloser) {
	rd1, wr1 := io.Pipe()
	rd2, wr2 := io.Pipe()
	client := &duplexPipe{rd1, wr2}
	server := &duplexPipe{rd2, wr1}
	return client, server
}

type muxadoAdaptor struct {
	Session
}

func (a *muxadoAdaptor) OpenStream() (muxStream, error) {
	return a.Session.OpenStream()
}

func (a *muxadoAdaptor) AcceptStream() (muxStream, error) {
	return a.Session.AcceptStream()
}

func newMuxadoAdaptor(rwc io.ReadWriteCloser, isServer bool) muxSession {
	newSess := Client
	if isServer {
		newSess = Server
	}
	return &muxadoAdaptor{newSess(rwc, new(Config))}
}

type yamuxAdaptor struct {
	*yamux.Session
}

func (a *yamuxAdaptor) OpenStream() (muxStream, error) {
	str, err := a.Session.OpenStream()
	return str, err
}

func (a *yamuxAdaptor) AcceptStream() (muxStream, error) {
	str, err := a.Session.AcceptStream()
	return str, err
}

func (a *yamuxAdaptor) Wait() (error, error, []byte) {
	select {}
}

func newYamuxAdaptor(rwc io.ReadWriteCloser, isServer bool) muxSession {
	newSess := yamux.Client
	if isServer {
		newSess = yamux.Server
	}
	sess, err := newSess(rwc, yamux.DefaultConfig())
	if err != nil {
		panic(err)
	}
	return &yamuxAdaptor{sess}
}

type sshAdaptor struct {
	ssh.Conn
	channels <-chan ssh.NewChannel
}

func (a *sshAdaptor) OpenStream() (muxStream, error) {
	c, reqs, err := a.Conn.OpenChannel("", []byte{})
	if err != nil {
		return nil, err
	}
	go ssh.DiscardRequests(reqs)
	return c, nil
}

func (a *sshAdaptor) AcceptStream() (muxStream, error) {
	newChannel, ok := <-a.channels
	if !ok {
		return nil, errors.New("SSH Session closed")
	}
	channel, reqs, err := newChannel.Accept()
	if err != nil {
		return nil, err
	}
	go ssh.DiscardRequests(reqs)
	return channel, nil
}

func (a *sshAdaptor) Wait() (error, error, []byte) {
	return a.Conn.Wait(), nil, nil
}

func newSSHAdaptor(rwc io.ReadWriteCloser, isServer bool) muxSession {
	var (
		conn           ssh.Conn
		newChannels    <-chan ssh.NewChannel
		globalRequests <-chan *ssh.Request
		err            error
	)
	if isServer {
		sconf := &ssh.ServerConfig{NoClientAuth: true}
		privKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		signer, err := ssh.NewSignerFromKey(privKey)
		if err != nil {
			panic(err)
		}
		sconf.AddHostKey(signer)
		conn, newChannels, globalRequests, err = ssh.NewServerConn(&rwcConn{rwc}, sconf)
	} else {
		conn, newChannels, globalRequests, err = ssh.NewClientConn(&rwcConn{rwc}, "", new(ssh.ClientConfig))
	}
	if err != nil {
		panic(err)
	}
	go ssh.DiscardRequests(globalRequests)
	return &sshAdaptor{conn, newChannels}
}

type rwcConn struct {
	io.ReadWriteCloser
}

func (c *rwcConn) LocalAddr() net.Addr              { return nil }
func (c *rwcConn) RemoteAddr() net.Addr             { return nil }
func (c *rwcConn) SetDeadline(time.Time) error      { return nil }
func (c *rwcConn) SetReadDeadline(time.Time) error  { return nil }
func (c *rwcConn) SetWriteDeadline(time.Time) error { return nil }

type tlsMode int

const (
	tlsModeClient tlsMode = iota
	tlsModeServer
	tlsModeNone
)

type tcpAdaptor struct {
	l          net.Listener
	remotePort string
	doTLS      tlsMode
	done       chan error
}

func (a *tcpAdaptor) OpenStream() (muxStream, error) {
	conn, err := net.Dial("tcp", "127.0.0.1:"+a.remotePort)
	if err != nil {
		return nil, err
	}
	return a.wrapTLS(conn), nil
}

func (a *tcpAdaptor) AcceptStream() (muxStream, error) {
	conn, err := a.l.Accept()
	if err != nil {
		return nil, err
	}
	return a.wrapTLS(conn), nil
}

func (a *tcpAdaptor) wrapTLS(c net.Conn) net.Conn {
	// XXX
	return c
}

func (a *tcpAdaptor) Wait() (error, error, []byte) {
	return <-a.done, nil, nil
}

// ===== LATENCY BENCHMARKS IMPLEMENTATION =====

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
	// {1024 * 1024, 1},      // 1MB  // Commented out - focus on 1KB payloads only
	// {32 * 1024 * 1024, 1}, // 32MB // Commented out - focus on 1KB payloads only

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

	sessFactory := newMuxadoAdaptor
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
